package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/getitdone-gmbh/agent-fabrik/internal/notify"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tools"
)

// notifyTool lets an agent tell its owner something that needs a human:
// a customer asks for a callback, information the agent needs is missing,
// something looks wrong. The message goes to the tenant's owner only, never
// to a visitor, and is capped per agent and day: a visitor can talk an
// agent into calling the tool, so it must not become a way to flood the
// owner's inbox.
type notifyTool struct {
	m      *Manager
	tenant string
}

const (
	maxNotifySubject = 120
	maxNotifyText    = 2000
)

func (n *notifyTool) Name() string { return "notify" }
func (n *notifyTool) Close() error { return nil }

func (n *notifyTool) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{{
		Local: "send",
		Description: "Sends a short message to the owner of this agent (by e-mail). Use it only when a human has to act: " +
			"a customer wants to be contacted, you lack information to answer, or something is broken. " +
			"Do not use it for routine answers, and never put secrets or full conversations in it. " +
			"Messages are limited per day.",
		Schema: tools.Schema(map[string]any{
			"subject":  tools.Prop("string", "one line, what this is about"),
			"message":  tools.Prop("string", "what happened and what the owner should do, plain text"),
			"severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "urgent"}, "description": "default info"},
		}, "subject", "message"),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Subject  string `json:"subject"`
				Message  string `json:"message"`
				Severity string `json:"severity"`
			}
			if err := tools.Decode(args, &a); err != nil {
				return "", err
			}
			a.Subject = strings.Join(strings.Fields(a.Subject), " ")
			a.Message = strings.TrimSpace(a.Message)
			if a.Subject == "" || a.Message == "" {
				return "", fmt.Errorf("subject and message are required")
			}
			switch a.Severity {
			case "", "info":
				a.Severity = "info"
			case "warning":
			case "urgent":
				a.Severity = "critical"
			default:
				return "", fmt.Errorf("severity must be info, warning or urgent")
			}
			call := tools.CallFrom(ctx)
			if !n.m.notifyLimiter.take(n.tenant+"/"+call.Agent, n.m.messagesPerDay(), time.Now()) {
				return "", fmt.Errorf("daily message limit reached; the owner was not notified")
			}
			err := n.m.notify(ctx, notify.Event{
				Type: notify.TypeMessage, Tenant: n.tenant, Agent: call.Agent, Severity: a.Severity,
				Subject: shorten(a.Subject, maxNotifySubject), Text: shorten(a.Message, maxNotifyText),
				RunID: call.RunID, At: time.Now().UTC(),
			})
			if err != nil {
				return "", fmt.Errorf("could not deliver the message: %w", err)
			}
			return "Message sent to the owner.", nil
		},
	}}, nil
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func (m *Manager) messagesPerDay() int {
	if n := m.cfg.Tenancy.Notify.MessagesPerDay; n > 0 {
		return n
	}
	return 5
}

// dayLimiter counts events per key in a sliding 24 hours. Per replica:
// the platform behind the notifier keeps its own cap across replicas.
type dayLimiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

func (l *dayLimiter) take(key string, limit int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string][]time.Time{}
	}
	cutoff := now.Add(-24 * time.Hour)
	keep := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= limit {
		l.seen[key] = keep
		return false
	}
	l.seen[key] = append(keep, now)
	// Drop keys that went quiet, so the map does not grow with every agent.
	if len(l.seen) > 10_000 {
		for k, ts := range l.seen {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(l.seen, k)
			}
		}
	}
	return true
}
