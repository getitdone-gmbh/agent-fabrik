// Package notify tells a tenant's owner about their agents: problems the
// alert checker found, and messages an agent sends with the notify tool.
// agent-fabrik does not send mail itself; it posts events to the operator's
// platform, which knows who owns a tenant and how to reach them.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Event types.
const (
	TypeAlert    = "alert"    // a problem started
	TypeResolved = "resolved" // the problem is gone
	TypeMessage  = "message"  // an agent asked to tell its owner something
)

// Event is posted as JSON to the configured URL.
type Event struct {
	Type     string    `json:"type"`
	Tenant   string    `json:"tenant"`
	Agent    string    `json:"agent"`
	Kind     string    `json:"kind,omitempty"`     // alert kind, see tenant/alerts.go
	Severity string    `json:"severity,omitempty"` // info, warning, critical
	Subject  string    `json:"subject"`
	Text     string    `json:"text"`
	RunID    string    `json:"run_id,omitempty"`
	At       time.Time `json:"at"`
}

// Notifier delivers events.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// Webhook posts events with a bearer token. The URL comes from the
// operator's config, never from a tenant, so it may point at an internal
// service.
type Webhook struct {
	URL, Token string
	Client     *http.Client
}

func (w *Webhook) Notify(ctx context.Context, e Event) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	c := w.Client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if w.Token != "" {
			req.Header.Set("Authorization", "Bearer "+w.Token)
		}
		resp, err := c.Do(req)
		if err != nil {
			last = err
			continue
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		if resp.StatusCode < 300 {
			return nil
		}
		last = fmt.Errorf("notify: HTTP %d: %s", resp.StatusCode, msg)
		if resp.StatusCode < 500 && resp.StatusCode != 429 {
			return last // our request is wrong; retrying will not help
		}
	}
	return last
}
