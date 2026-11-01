package store

import (
	"context"
	"encoding/json"
	"sort"
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
	ErrorKind    string      `json:"error_kind,omitempty"` // auth, network, timeout, server, throttled, client, other
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
	Agent string
	// SessionID selects one conversation. Its runs come with the full
	// question and answer (no preview), so the conversation can be read.
	SessionID  string
	Status     string    // "", ok, max_steps, error, or "failed" (anything but ok)
	Before     time.Time // older than this (paging)
	Limit      int
	AllTenants bool // admin overview
}

// RunStore keeps run records.
type RunStore interface {
	RecordRun(ctx context.Context, r Run) error
	// ListRuns returns runs newest first, without trace and with input and
	// output shortened to a preview (full texts with f.SessionID).
	ListRuns(ctx context.Context, tenant string, f RunFilter) ([]Run, error)
	// ListSessions groups a tenant's runs by conversation, newest first.
	ListSessions(ctx context.Context, tenant string, f SessionFilter) ([]SessionSummary, error)
	GetRun(ctx context.Context, tenant, id string) (*Run, error)
	// PruneRuns deletes runs started before t and returns how many.
	PruneRuns(ctx context.Context, before time.Time) (int64, error)

	// RunStats aggregates a tenant's runs since t, per day and per agent
	// (agent "" = all agents).
	RunStats(ctx context.Context, tenant, agent string, since time.Time) (*RunStats, error)
	// RunHealth aggregates the runs of every tenant and agent since t, for
	// the alert checker.
	RunHealth(ctx context.Context, since time.Time) ([]AgentHealth, error)

	// Alerts are the problems the checker found and has notified about.
	ListAlerts(ctx context.Context, tenant string, all bool) ([]Alert, error)
	PutAlert(ctx context.Context, a Alert) error
	DeleteAlert(ctx context.Context, tenant, agent, kind string) error
}

// RunTotals sums up runs.
type RunTotals struct {
	Runs   int64 `json:"runs"`
	OK     int64 `json:"ok"`
	Failed int64 `json:"failed"` // error or max_steps
	// What OK and Failed are made of, so a page can say what "successful"
	// means: OK = Clean + OKToolErrors, Failed = MaxSteps + ModelErrors + OtherErrors.
	Clean        int64 `json:"clean"`          // answered, every tool call worked
	OKToolErrors int64 `json:"ok_tool_errors"` // answered although a tool call failed
	MaxSteps     int64 `json:"max_steps"`      // stopped at the step limit, no answer
	ModelErrors  int64 `json:"model_errors"`   // the model rejected the key, was unreachable or throttled
	OtherErrors  int64 `json:"other_errors"`
	ToolCalls    int64 `json:"tool_calls"`
	ToolErrors   int64 `json:"tool_errors"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	AvgMS        int64 `json:"avg_ms"`
	P95MS        int64 `json:"p95_ms"`
}

// DayStats is one UTC day.
type DayStats struct {
	Date string `json:"date"` // YYYY-MM-DD
	RunTotals
}

// AgentStats is one agent's totals.
type AgentStats struct {
	Agent string `json:"agent"`
	RunTotals
	LastRunAt time.Time `json:"last_run_at"`
}

// RunStats is the answer of GET /v1/stats.
type RunStats struct {
	Since  time.Time    `json:"since"`
	Totals RunTotals    `json:"totals"`
	Days   []DayStats   `json:"days"`
	Agents []AgentStats `json:"agents"`
}

// AgentHealth is what the alert checker looks at for one agent.
type AgentHealth struct {
	TenantID     string
	Agent        string
	Runs         int64
	OK           int64
	Failed       int64
	ToolCalls    int64
	ToolErrors   int64
	Tokens       int64
	AuthErrors   int64 // model rejected the key
	ReachErrors  int64 // model unreachable: network, timeout, server
	LastFailedID string
	LastError    string
}

// Alert is an active problem of one agent.
type Alert struct {
	TenantID string    `json:"tenant_id"`
	Agent    string    `json:"agent"`
	Kind     string    `json:"kind"` // runs_failing, tool_errors, llm_auth, llm_unreachable, token_spike
	Severity string    `json:"severity"`
	Summary  string    `json:"summary"`
	RunID    string    `json:"run_id,omitempty"`
	Since    time.Time `json:"since"`
	LastSeen time.Time `json:"last_seen"`
}

// Health applies the run log's error classes to a run.
func (r *Run) health(h *AgentHealth) {
	h.Runs++
	if r.Status == RunOK {
		h.OK++
	} else {
		h.Failed++
		if h.LastFailedID == "" {
			h.LastFailedID, h.LastError = r.ID, r.Error
		}
	}
	h.ToolCalls += int64(r.ToolCalls)
	h.ToolErrors += int64(r.ToolErrors)
	h.Tokens += int64(max(r.InputTokens, 0) + max(r.OutputTokens, 0))
	switch r.ErrorKind {
	case "auth":
		h.AuthErrors++
	case "network", "timeout", "server":
		h.ReachErrors++
	}
}

func (t *RunTotals) add(r *Run) {
	t.Runs++
	switch {
	case r.Status == RunOK:
		t.OK++
		if r.ToolErrors > 0 {
			t.OKToolErrors++
		} else {
			t.Clean++
		}
	case r.Status == RunMaxSteps:
		t.Failed++
		t.MaxSteps++
	default:
		t.Failed++
		switch r.ErrorKind {
		case "auth", "network", "timeout", "server", "throttled":
			t.ModelErrors++
		default:
			t.OtherErrors++
		}
	}
	t.ToolCalls += int64(r.ToolCalls)
	t.ToolErrors += int64(r.ToolErrors)
	t.InputTokens += int64(max(r.InputTokens, 0))
	t.OutputTokens += int64(max(r.OutputTokens, 0))
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
	if f.SessionID != "" && r.SessionID != f.SessionID {
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

// finish sets AvgMS and P95MS from the durations of the runs counted.
func (t *RunTotals) finish(ms []int64) {
	if len(ms) == 0 {
		return
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
	var sum int64
	for _, d := range ms {
		sum += d
	}
	t.AvgMS = sum / int64(len(ms))
	t.P95MS = ms[(len(ms)*95+99)/100-1]
}

// statsOf aggregates runs (any order) the same way both backends do.
func statsOf(runs []Run, since time.Time) *RunStats {
	st := &RunStats{Since: since, Days: []DayStats{}, Agents: []AgentStats{}}
	var all []int64
	days := map[string]*DayStats{}
	dayMS := map[string][]int64{}
	agents := map[string]*AgentStats{}
	agentMS := map[string][]int64{}
	for i := range runs {
		r := &runs[i]
		st.Totals.add(r)
		all = append(all, r.DurationMS)
		d := r.StartedAt.UTC().Format("2006-01-02")
		if days[d] == nil {
			days[d] = &DayStats{Date: d}
		}
		days[d].add(r)
		dayMS[d] = append(dayMS[d], r.DurationMS)
		if agents[r.Agent] == nil {
			agents[r.Agent] = &AgentStats{Agent: r.Agent}
		}
		a := agents[r.Agent]
		a.add(r)
		if r.StartedAt.After(a.LastRunAt) {
			a.LastRunAt = r.StartedAt
		}
		agentMS[r.Agent] = append(agentMS[r.Agent], r.DurationMS)
	}
	st.Totals.finish(all)
	for d, s := range days {
		s.finish(dayMS[d])
		st.Days = append(st.Days, *s)
	}
	sort.Slice(st.Days, func(i, j int) bool { return st.Days[i].Date < st.Days[j].Date })
	for n, a := range agents {
		a.finish(agentMS[n])
		st.Agents = append(st.Agents, *a)
	}
	sort.Slice(st.Agents, func(i, j int) bool { return st.Agents[i].Agent < st.Agents[j].Agent })
	return st
}

// SessionFilter selects conversations.
type SessionFilter struct {
	Agent  string
	Failed bool      // only conversations with at least one failed run
	Before time.Time // last run older than this (paging)
	Limit  int
}

func (f SessionFilter) limit() int { return RunFilter{Limit: f.Limit}.limit() }

// SessionSummary is one conversation in the run log.
type SessionSummary struct {
	SessionID  string    `json:"session_id"`
	Agent      string    `json:"agent"`
	Runs       int64     `json:"runs"`
	Failed     int64     `json:"failed"`
	FirstInput string    `json:"first_input"` // preview of the opening question
	FirstAt    time.Time `json:"first_at"`
	LastAt     time.Time `json:"last_at"`
}

// sessionsOf groups runs (any order) the way both backends do.
func sessionsOf(runs []Run, f SessionFilter) []SessionSummary {
	by := map[string]*SessionSummary{}
	for i := range runs {
		r := &runs[i]
		if r.SessionID == "" || (f.Agent != "" && r.Agent != f.Agent) {
			continue
		}
		k := r.Agent + "/" + r.SessionID
		s := by[k]
		if s == nil {
			s = &SessionSummary{SessionID: r.SessionID, Agent: r.Agent, FirstAt: r.StartedAt, LastAt: r.StartedAt, FirstInput: clip(r.Input, previewLen)}
			by[k] = s
		}
		s.Runs++
		if r.Status != RunOK {
			s.Failed++
		}
		if r.StartedAt.Before(s.FirstAt) {
			s.FirstAt, s.FirstInput = r.StartedAt, clip(r.Input, previewLen)
		}
		if r.StartedAt.After(s.LastAt) {
			s.LastAt = r.StartedAt
		}
	}
	out := []SessionSummary{}
	for _, s := range by {
		if (f.Failed && s.Failed == 0) || (!f.Before.IsZero() && !s.LastAt.Before(f.Before)) {
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAt.After(out[j].LastAt) })
	if len(out) > f.limit() {
		out = out[:f.limit()]
	}
	return out
}
