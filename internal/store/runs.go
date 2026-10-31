package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Run is the record of one agent run: what it was asked, what it did and
// what it answered. It is kept for a limited time (see PruneRuns) so an
// operator can check that an agent does what it should.
type Run struct {
	ID           string      `json:"id"`
	TenantID     string      `json:"tenant_id"`
	Agent        string      `json:"agent"`
	Source       string      `json:"source"` // api, chat, mcp, webhook, cron, imap
	SessionID    string      `json:"session_id,omitempty"`
	Status       string      `json:"status"` // ok, max_steps, error
	Error        string      `json:"error,omitempty"`
	Input        string      `json:"input"`
	Images       int         `json:"images,omitempty"` // attached images (not stored)
	Output       string      `json:"output"`
	Trace        []TraceStep `json:"trace,omitempty"` // only in GetRun
	Steps        int         `json:"steps"`
	ToolCalls    int         `json:"tool_calls"`
	ToolErrors   int         `json:"tool_errors"`
	InputTokens  int         `json:"input_tokens"`
	OutputTokens int         `json:"output_tokens"`
	DurationMS   int64       `json:"duration_ms"`
	StartedAt    time.Time   `json:"started_at"`
}

// TraceStep is one event of a run (tool call, tool result, model message).
type TraceStep struct {
	Type    string          `json:"type"`
	Step    int             `json:"step,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Text    string          `json:"text,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// Run statuses.
const (
	RunOK       = "ok"
	RunMaxSteps = "max_steps"
	RunError    = "error"
)

// RunFilter selects runs of one tenant ("" or "*" with AllTenants).
type RunFilter struct {
	Agent      string
	Status     string    // "", ok, max_steps, error, or "failed" (anything but ok)
	Before     time.Time // older than this (paging)
	Limit      int
	AllTenants bool // admin overview
}

// RunStore keeps run records.
type RunStore interface {
	RecordRun(ctx context.Context, r Run) error
	// ListRuns returns runs newest first, without trace and with input and
	// output shortened to a preview.
	ListRuns(ctx context.Context, tenant string, f RunFilter) ([]Run, error)
	GetRun(ctx context.Context, tenant, id string) (*Run, error)
	// PruneRuns deletes runs started before t and returns how many.
	PruneRuns(ctx context.Context, before time.Time) (int64, error)
}

// Size caps for a stored run. A run may carry 32 MiB of input and tool
// results of 20k characters each; the record only has to show what happened.
const (
	maxRunText    = 16 << 10
	maxTraceText  = 4 << 10
	maxTraceSteps = 200
	previewLen    = 300
)

// Bound returns r with every text shortened to the stored limits and NUL
// bytes removed (PostgreSQL text and jsonb reject them).
func (r Run) Bound() Run {
	r.Input, r.Output, r.Error = clip(r.Input, maxRunText), clip(r.Output, maxRunText), clip(r.Error, maxTraceText)
	if len(r.Trace) > maxTraceSteps {
		r.Trace = append(r.Trace[:maxTraceSteps:maxTraceSteps], TraceStep{Type: "note", Text: "trace truncated"})
	}
	tr := make([]TraceStep, len(r.Trace))
	for i, s := range r.Trace {
		s.Text = clip(s.Text, maxTraceText)
		if len(s.Args) > maxTraceText || !json.Valid(s.Args) || strings.Contains(string(s.Args), `\u0000`) {
			if len(s.Args) > 0 {
				s.Args, _ = json.Marshal(clip(string(s.Args), maxTraceText))
			}
		}
		tr[i] = s
	}
	r.Trace = tr
	return r
}

// Preview shortens a run for listings.
func (r Run) Preview() Run {
	r.Trace = nil
	r.Input, r.Output = clip(r.Input, previewLen), clip(r.Output, previewLen)
	return r
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func (f RunFilter) limit() int {
	switch {
	case f.Limit <= 0:
		return 50
	case f.Limit > 200:
		return 200
	}
	return f.Limit
}

func (f RunFilter) match(r *Run) bool {
	if f.Agent != "" && r.Agent != f.Agent {
		return false
	}
	switch f.Status {
	case "":
	case "failed":
		if r.Status == RunOK {
			return false
		}
	default:
		if r.Status != f.Status {
			return false
		}
	}
	return f.Before.IsZero() || r.StartedAt.Before(f.Before)
}
