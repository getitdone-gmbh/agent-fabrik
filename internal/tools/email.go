package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"path"
	"strings"
	"sync"
	"time"

	imail "github.com/getitdone-gmbh/agentd/internal/mail"
)

// EmailConfig configures an email connector (SMTP, IMAP, or both).
type EmailConfig struct {
	SMTP *imail.SMTPConfig `yaml:"smtp"`
	IMAP *imail.IMAPConfig `yaml:"imap"`
	// AllowRecipients restricts send_email to matching addresses or globs
	// like "*@example.com". Empty means any recipient.
	AllowRecipients []string `yaml:"allow_recipients"`
	// ReadOnly disables mark_read (IMAP).
	ReadOnly bool `yaml:"read_only"`
}

// EmailConnector exposes send/list/read tools over SMTP and IMAP.
type EmailConnector struct {
	name string
	cfg  EmailConfig
	mu   sync.Mutex // serialize IMAP sessions
}

func NewEmailConnector(name string, cfg EmailConfig) (*EmailConnector, error) {
	if cfg.SMTP == nil && cfg.IMAP == nil {
		return nil, fmt.Errorf("email connector %q: configure smtp and/or imap", name)
	}
	if cfg.SMTP != nil && (cfg.SMTP.Host == "" || cfg.SMTP.From == "") {
		return nil, fmt.Errorf("email connector %q: smtp.host and smtp.from are required", name)
	}
	if cfg.IMAP != nil && cfg.IMAP.Host == "" {
		return nil, fmt.Errorf("email connector %q: imap.host is required", name)
	}
	return &EmailConnector{name: name, cfg: cfg}, nil
}

func (e *EmailConnector) Name() string        { return e.name }
func (e *EmailConnector) Close() error        { return nil }
func (e *EmailConnector) Config() EmailConfig { return e.cfg }

func (e *EmailConnector) recipientAllowed(addr string) bool {
	if len(e.cfg.AllowRecipients) == 0 {
		return true
	}
	a, err := mail.ParseAddress(addr)
	if err != nil {
		return false
	}
	lower := strings.ToLower(a.Address)
	for _, p := range e.cfg.AllowRecipients {
		if ok, _ := path.Match(strings.ToLower(p), lower); ok {
			return true
		}
	}
	return false
}

// Send sends mail through the connector's SMTP settings.
func (e *EmailConnector) Send(ctx context.Context, m imail.Outgoing) (string, error) {
	if e.cfg.SMTP == nil {
		return "", errors.New("smtp is not configured for this connector")
	}
	for _, list := range [][]string{m.To, m.Cc, m.Bcc} {
		for _, r := range list {
			for _, one := range strings.Split(r, ",") {
				if strings.TrimSpace(one) != "" && !e.recipientAllowed(one) {
					return "", fmt.Errorf("recipient %q is not allowed", strings.TrimSpace(one))
				}
			}
		}
	}
	return imail.Send(ctx, *e.cfg.SMTP, m)
}

// WithIMAP runs fn with a logged-in IMAP session on the given mailbox.
func (e *EmailConnector) WithIMAP(ctx context.Context, mailbox string, fn func(c *imail.IMAPClient) error) error {
	if e.cfg.IMAP == nil {
		return errors.New("imap is not configured for this connector")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	c, err := imail.DialIMAP(ctx, *e.cfg.IMAP)
	if err != nil {
		return err
	}
	defer c.Close()
	if mailbox == "" {
		mailbox = e.cfg.IMAP.Mailbox
	}
	if _, err := c.Select(mailbox); err != nil {
		return err
	}
	return fn(c)
}

func (e *EmailConnector) Tools(context.Context) ([]Tool, error) {
	var out []Tool
	if e.cfg.SMTP != nil {
		out = append(out, Tool{
			Local:       "send_email",
			Description: "Sends a plain-text email from " + e.cfg.SMTP.From + ". To reply to a message pass its message_id as in_reply_to.",
			Schema: Schema(map[string]any{
				"to":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Recipient addresses"},
				"cc":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"subject":     Prop("string", "Subject line"),
				"body":        Prop("string", "Plain-text body"),
				"in_reply_to": Prop("string", "Message-ID being replied to (optional)"),
			}, "to", "subject", "body"),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					To        []string `json:"to"`
					Cc        []string `json:"cc"`
					Subject   string   `json:"subject"`
					Body      string   `json:"body"`
					InReplyTo string   `json:"in_reply_to"`
				}
				if err := Decode(args, &a); err != nil {
					return "", err
				}
				id, err := e.Send(ctx, imail.Outgoing{To: a.To, Cc: a.Cc, Subject: a.Subject, Text: a.Body, InReplyTo: a.InReplyTo, References: a.InReplyTo, AutoSubmitted: "auto-generated"})
				if err != nil {
					return "", err
				}
				return "sent, message_id " + id, nil
			},
		})
	}
	if e.cfg.IMAP != nil {
		out = append(out,
			Tool{
				Local:       "list_emails",
				Description: "Lists emails in a mailbox (newest first) with uid, sender, subject and date. Use read_email to get the body.",
				Schema: Schema(map[string]any{
					"mailbox":     Prop("string", "Mailbox name, default INBOX"),
					"unread_only": Prop("boolean", "Only unread messages"),
					"from":        Prop("string", "Filter by sender (substring)"),
					"subject":     Prop("string", "Filter by subject (substring)"),
					"text":        Prop("string", "Full-text search"),
					"since":       Prop("string", "Only messages since this date (YYYY-MM-DD)"),
					"limit":       Prop("integer", "Max results, default 20"),
				}),
				Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
					var a struct {
						Mailbox    string `json:"mailbox"`
						UnreadOnly bool   `json:"unread_only"`
						From       string `json:"from"`
						Subject    string `json:"subject"`
						Text       string `json:"text"`
						Since      string `json:"since"`
						Limit      int    `json:"limit"`
					}
					if err := Decode(args, &a); err != nil {
						return "", err
					}
					sc := imail.SearchCriteria{Unseen: a.UnreadOnly, From: a.From, Subject: a.Subject, Text: a.Text}
					if a.Since != "" {
						t, err := time.Parse("2006-01-02", a.Since)
						if err != nil {
							return "", fmt.Errorf("since: %w", err)
						}
						sc.Since = t
					}
					if a.Limit <= 0 || a.Limit > 100 {
						a.Limit = 20
					}
					var res []imail.Summary
					err := e.WithIMAP(ctx, a.Mailbox, func(c *imail.IMAPClient) error {
						uids, err := c.Search(sc)
						if err != nil {
							return err
						}
						if len(uids) > a.Limit {
							uids = uids[len(uids)-a.Limit:]
						}
						res, err = c.FetchSummaries(uids)
						return err
					})
					if err != nil {
						return "", err
					}
					// newest first
					for i, j := 0, len(res)-1; i < j; i, j = i+1, j-1 {
						res[i], res[j] = res[j], res[i]
					}
					if len(res) == 0 {
						return "no messages", nil
					}
					return JSON(res), nil
				},
			},
			Tool{
				Local:       "read_email",
				Description: "Reads one email by uid and returns headers and plain-text body.",
				Schema: Schema(map[string]any{
					"uid":     Prop("integer", "Message UID from list_emails"),
					"mailbox": Prop("string", "Mailbox name, default INBOX"),
				}, "uid"),
				Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
					var a struct {
						UID     uint32 `json:"uid"`
						Mailbox string `json:"mailbox"`
					}
					if err := Decode(args, &a); err != nil {
						return "", err
					}
					var em *imail.Email
					err := e.WithIMAP(ctx, a.Mailbox, func(c *imail.IMAPClient) error {
						raw, err := c.FetchRaw(a.UID)
						if err != nil {
							return err
						}
						em, err = imail.ParseEmail(raw)
						return err
					})
					if err != nil {
						return "", err
					}
					em.UID = a.UID
					if len(em.Text) > 20000 {
						em.Text = em.Text[:20000] + "\n…(truncated)"
					}
					return JSON(em), nil
				},
			},
		)
		if !e.cfg.ReadOnly {
			out = append(out, Tool{
				Local:       "mark_read",
				Description: "Marks emails as read.",
				Schema: Schema(map[string]any{
					"uids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
					"mailbox": Prop("string", "Mailbox name, default INBOX"),
				}, "uids"),
				Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
					var a struct {
						UIDs    []uint32 `json:"uids"`
						Mailbox string   `json:"mailbox"`
					}
					if err := Decode(args, &a); err != nil {
						return "", err
					}
					err := e.WithIMAP(ctx, a.Mailbox, func(c *imail.IMAPClient) error { return c.AddFlags(a.UIDs, `\Seen`) })
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("marked %d message(s) as read", len(a.UIDs)), nil
				},
			})
		}
	}
	return out, nil
}
