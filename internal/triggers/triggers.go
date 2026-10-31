// Package triggers starts agent runs from schedules and mailboxes. A single
// heap-based scheduler serves the triggers of all tenants.
package triggers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"path"
	"strings"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/agent"
	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	imail "github.com/getitdone-gmbh/agent-fabrik/internal/mail"
	"github.com/getitdone-gmbh/agent-fabrik/internal/metrics"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tools"
)

// NoReply is the answer an agent gives to suppress an email reply.
const NoReply = "NO_REPLY"

// Env is what a trigger needs from the tenant it belongs to.
type Env interface {
	Run(ctx context.Context, agentName string, in agent.RunInput) (*agent.RunResult, error)
	// Email returns an email connector and a release func.
	Email(ctx context.Context, connector string) (*tools.EmailConnector, func(), error)
}

// expandPrompt substitutes {{now}} and {{date}} placeholders.
func expandPrompt(p string, now time.Time) string {
	return strings.NewReplacer("{{now}}", now.Format(time.RFC3339), "{{date}}", now.Format("2006-01-02")).Replace(p)
}

func runScheduled(ctx context.Context, env Env, agentName string, t config.Trigger, at time.Time, log *slog.Logger) {
	prompt := t.Prompt
	if prompt == "" {
		prompt = "Scheduled run at {{now}}. Perform your routine task."
	}
	metrics.TriggerRuns.Add(1)
	res, err := env.Run(ctx, agentName, agent.RunInput{Input: expandPrompt(prompt, at), SessionID: t.Session, Source: t.Type})
	if err != nil {
		if ctx.Err() == nil { // not a shutdown
			log.Warn("scheduled run failed", "err", err)
		}
		return
	}
	log.Info("scheduled run done", "session", res.SessionID, "output_chars", len(res.Output))
}

// ---------- IMAP poller ----------

type poller struct {
	agent    string
	t        config.Trigger
	log      *slog.Logger
	attempts map[uint32]int
}

const maxAttempts = 3

type inbound struct {
	uid   uint32
	email *imail.Email
}

func (p *poller) poll(ctx context.Context, env Env) error {
	conn, release, err := env.Email(ctx, p.t.Connector)
	if err != nil {
		return err
	}
	defer release()
	reply := p.t.Reply == nil || *p.t.Reply
	if reply && conn.Config().SMTP == nil {
		reply = false
	}
	max := p.t.MaxPerPoll
	if max <= 0 {
		max = 5 // the tenant cap (config.TenantMaxPerPoll); operators may set more
	}
	var batch []inbound
	var skip []uint32
	// Fetch first, then release the mailbox before running agents so the
	// agent's own email tools can use the same connector.
	err = conn.WithIMAP(ctx, p.t.Mailbox, func(c *imail.IMAPClient) error {
		uids, err := c.Search(imail.SearchCriteria{Unseen: true})
		if err != nil {
			return err
		}
		if len(uids) > max {
			uids = uids[:max]
		}
		for _, uid := range uids {
			e, err := c.FetchEmail(uid)
			if errors.Is(err, imail.ErrUnparseable) {
				p.log.Warn("unparseable email, skipping", "uid", uid, "err", err)
				skip = append(skip, uid)
				continue
			}
			if err != nil {
				return err
			}
			if e.IsAutomated() || !p.senderAllowed(e.From) {
				p.log.Info("ignoring email", "uid", uid, "from", e.From, "automated", e.IsAutomated())
				skip = append(skip, uid)
				continue
			}
			batch = append(batch, inbound{uid: uid, email: e})
		}
		return nil
	})
	if err != nil {
		return err
	}

	done := append([]uint32(nil), skip...)
	for _, in := range batch {
		if ctx.Err() != nil {
			break
		}
		if err := p.handle(ctx, env, conn, in.email, reply); err != nil {
			if errors.Is(err, agent.ErrThrottled) {
				// Throttling is not the message's fault: leave it (and the
				// rest of the batch) unread for a later poll.
				p.log.Warn("tenant throttled, leaving emails unread", "err", err)
				break
			}
			p.attempts[in.uid]++
			p.log.Error("handling email failed", "uid", in.uid, "attempt", p.attempts[in.uid], "err", err)
			if p.attempts[in.uid] < maxAttempts {
				continue
			}
			p.log.Error("giving up on email", "uid", in.uid)
		}
		delete(p.attempts, in.uid)
		done = append(done, in.uid)
	}
	if len(done) == 0 || (p.t.MarkRead != nil && !*p.t.MarkRead) {
		return nil
	}
	return conn.WithIMAP(context.WithoutCancel(ctx), p.t.Mailbox, func(c *imail.IMAPClient) error {
		return c.AddFlags(done, `\Seen`)
	})
}

func (p *poller) senderAllowed(from string) bool {
	if len(p.t.AllowFrom) == 0 {
		return true
	}
	a, err := mail.ParseAddress(from)
	if err != nil {
		return false
	}
	addr := strings.ToLower(a.Address)
	for _, pat := range p.t.AllowFrom {
		if ok, _ := path.Match(strings.ToLower(pat), addr); ok {
			return true
		}
	}
	return false
}

// threadSession maps an email thread to a stable session id. The sender is
// part of it: thread ids come from headers anyone can write, and knowing a
// Message-ID must not be enough to read on in someone else's conversation.
func threadSession(e *imail.Email) string {
	root := e.MessageID
	if refs := strings.Fields(e.References); len(refs) > 0 {
		root = refs[0]
	} else if e.InReplyTo != "" {
		root = e.InReplyTo
	}
	if root == "" {
		return ""
	}
	from := e.From
	if a, err := mail.ParseAddress(e.From); err == nil {
		from = a.Address
	}
	sum := sha256.Sum256([]byte(strings.ToLower(from) + "\x00" + strings.Trim(root, "<>")))
	return "mail:" + hex.EncodeToString(sum[:16])
}

func (p *poller) handle(ctx context.Context, env Env, conn *tools.EmailConnector, e *imail.Email, reply bool) error {
	body := e.Text
	if len(body) > 30000 {
		body = body[:30000] + "\n…(truncated)"
	}
	var b strings.Builder
	if p.t.Prompt != "" {
		b.WriteString(expandPrompt(p.t.Prompt, time.Now()) + "\n\n")
	}
	b.WriteString("New email received.\n")
	fmt.Fprintf(&b, "From: %s\nTo: %s\n", e.From, e.To)
	if e.Cc != "" {
		fmt.Fprintf(&b, "Cc: %s\n", e.Cc)
	}
	fmt.Fprintf(&b, "Subject: %s\nDate: %s\nMessage-ID: %s\n", e.Subject, e.Date, e.MessageID)
	if len(e.Attachments) > 0 {
		fmt.Fprintf(&b, "Attachments: %s\n", strings.Join(e.Attachments, ", "))
	}
	b.WriteString("\n" + body + "\n")
	if reply {
		b.WriteString("\nYour final answer will be sent as the email reply body. Answer exactly " + NoReply + " if no reply should be sent.")
	}

	metrics.TriggerRuns.Add(1)
	res, err := env.Run(ctx, p.agent, agent.RunInput{Input: b.String(), SessionID: threadSession(e), Source: "imap"})
	if err != nil {
		return err
	}
	out := strings.TrimSpace(res.Output)
	if !reply || out == "" || out == NoReply || res.StopReason == "max_steps" {
		p.log.Info("email handled without reply", "uid", e.UID, "stop", res.StopReason)
		return nil
	}
	// From is what allow_from checked; a Reply-To outside the allowlist
	// would let anyone who forges an allowed From read the agent's answer.
	to := e.ReplyTo
	if to == "" || !p.senderAllowed(to) {
		to = e.From
	}
	subject := e.Subject
	if !strings.HasPrefix(strings.ToLower(subject), "re:") {
		subject = "Re: " + subject
	}
	refs := strings.TrimSpace(e.References + " " + e.MessageID)
	id, err := conn.Send(ctx, imail.Outgoing{To: []string{to}, Subject: subject, Text: out, InReplyTo: e.MessageID, References: refs, AutoSubmitted: "auto-replied"})
	if err != nil {
		return fmt.Errorf("send reply: %w", err)
	}
	p.log.Info("replied to email", "uid", e.UID, "to", to, "message_id", id)
	return nil
}
