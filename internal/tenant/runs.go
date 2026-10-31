package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/agent"
	"github.com/getitdone-gmbh/agent-fabrik/internal/metrics"
	"github.com/getitdone-gmbh/agent-fabrik/internal/store"
)

// recorder collects what a run does, for the run log and the metrics.
type recorder struct {
	run     store.Run
	mu      sync.Mutex
	started bool // the model was called at least once
}

func (m *Manager) newRecorder(tenantID, agentName string, in agent.RunInput) *recorder {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	src := in.Source
	if src == "" {
		src = "api"
	}
	return &recorder{run: store.Run{
		ID: "run_" + hex.EncodeToString(b), TenantID: tenantID, Agent: agentName, Source: src,
		Input: in.Input, Images: len(in.Images), StartedAt: time.Now().UTC(),
	}}
}

// wrap returns an event handler that records the event and passes it on.
func (r *recorder) wrap(next func(agent.Event)) func(agent.Event) {
	return func(e agent.Event) {
		r.mu.Lock()
		switch e.Type {
		case "step":
			r.started = true
		case "tool_call":
			r.run.Trace = append(r.run.Trace, store.TraceStep{Type: e.Type, Step: e.Step, Tool: e.Tool, CallID: e.CallID, Args: e.Args})
		case "tool_result":
			r.run.Trace = append(r.run.Trace, store.TraceStep{Type: e.Type, Step: e.Step, Tool: e.Tool, CallID: e.CallID, Text: e.Text, IsError: e.IsError})
			if e.IsError {
				r.run.ToolErrors++
			}
		case "message", "error":
			r.run.Trace = append(r.run.Trace, store.TraceStep{Type: e.Type, Step: e.Step, Text: e.Text, IsError: e.Type == "error"})
		}
		r.mu.Unlock()
		if next != nil {
			next(e)
		}
	}
}

// runLogRetention is how long run records are kept; 0 disables the log.
func (m *Manager) runLogRetention() time.Duration {
	d := m.cfg.Tenancy.RunLogDays
	switch {
	case m.st == nil || d < 0:
		return 0
	case d == 0:
		d = 30
	}
	return time.Duration(d) * 24 * time.Hour
}

// finish records a run that reached the model. Runs refused before that
// (rate limit, quota, unknown agent) only count in the metrics: they did
// nothing worth looking at, and anyone with a key could fill the log.
func (m *Manager) finish(r *recorder, res *agent.RunResult, err error) {
	tenantLabel := r.run.TenantID
	switch {
	case errors.Is(err, ErrRateLimited):
		metrics.Rejected.Inc(tenantLabel, "rate_limit")
		return
	case errors.Is(err, ErrQuotaExceeded):
		metrics.Rejected.Inc(tenantLabel, "quota")
		return
	case errors.Is(err, ErrDisabled):
		metrics.Rejected.Inc(tenantLabel, "disabled")
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.started {
		return
	}
	run := &r.run
	run.Status = store.RunOK
	if res != nil {
		run.SessionID, run.Output, run.Steps, run.ToolCalls = res.SessionID, res.Output, res.Steps, res.ToolCalls
		run.InputTokens, run.OutputTokens, run.DurationMS = res.Usage.InputTokens, res.Usage.OutputTokens, res.DurationMS
		if res.StopReason == "max_steps" {
			run.Status = store.RunMaxSteps
		}
	}
	if run.DurationMS == 0 {
		run.DurationMS = time.Since(run.StartedAt).Milliseconds()
	}
	if err != nil {
		run.Status, run.Error, run.ErrorKind = store.RunError, err.Error(), "other"
		if res != nil && res.ErrorKind != "" {
			run.ErrorKind = res.ErrorKind
		}
	}

	a := run.Agent
	metrics.Runs.Inc(tenantLabel, a, run.Source, run.Status)
	metrics.RunDuration.Observe(float64(run.DurationMS)/1000, tenantLabel, a)
	metrics.RunSteps.Add(float64(run.Steps), tenantLabel, a)
	metrics.Tokens.Add(float64(max(run.InputTokens, 0)), tenantLabel, a, "input")
	metrics.Tokens.Add(float64(max(run.OutputTokens, 0)), tenantLabel, a, "output")
	for _, s := range run.Trace {
		if s.Type == "tool_result" {
			result := "ok"
			if s.IsError {
				result = "error"
			}
			metrics.Tools.Inc(tenantLabel, a, s.Tool, result)
		}
	}

	if m.runLogRetention() == 0 {
		return
	}
	// Not the request's context: a client that hung up still ran the agent.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.st.RecordRun(ctx, *run); err != nil {
		m.log.Warn("recording run failed", "tenant", run.TenantID, "agent", a, "err", err)
	}
}

// pruneRuns drops run records past the retention. Called by the leader.
func (m *Manager) pruneRuns(ctx context.Context) {
	keep := m.runLogRetention()
	if keep == 0 {
		return
	}
	n, err := m.st.PruneRuns(ctx, time.Now().Add(-keep))
	if err != nil {
		m.log.Warn("pruning run log failed", "err", err)
	} else if n > 0 {
		m.log.Info("pruned run log", "deleted", n)
	}
}

// Runs lists a tenant's run records (all tenants for the admin overview).
func (m *Manager) Runs(ctx context.Context, tenantID string, f store.RunFilter) ([]store.Run, error) {
	if m.st == nil {
		return []store.Run{}, nil
	}
	return m.st.ListRuns(ctx, tenantID, f)
}

// GetRun returns one run record with its trace.
func (m *Manager) GetRun(ctx context.Context, tenantID, id string) (*store.Run, error) {
	if m.st == nil {
		return nil, store.ErrNotFound
	}
	return m.st.GetRun(ctx, tenantID, id)
}

// Sessions lists a tenant's conversations from the run log.
func (m *Manager) Sessions(ctx context.Context, tenantID string, f store.SessionFilter) ([]store.SessionSummary, error) {
	if m.st == nil {
		return []store.SessionSummary{}, nil
	}
	return m.st.ListSessions(ctx, tenantID, f)
}

// RunLogDays is how many days the run log keeps (0: off).
func (m *Manager) RunLogDays() int { return int(m.runLogRetention().Hours() / 24) }
