package tenant

import (
	"context"
	"fmt"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/notify"
	"github.com/getitdone-gmbh/agent-fabrik/internal/store"
)

// The alert checker looks at the run log every few minutes and tells an
// agent's owner when it stops doing its job. Same rules as the operator's
// Prometheus alerts, but per tenant and delivered to the tenant.

// Alert kinds.
const (
	AlertRunsFailing    = "runs_failing"
	AlertToolErrors     = "tool_errors"
	AlertLLMAuth        = "llm_auth"
	AlertLLMUnreachable = "llm_unreachable"
	AlertTokenSpike     = "token_spike"
)

const (
	alertInterval = 5 * time.Minute
	// An alert nobody's runs confirm or clear any more (the agent is not
	// used) is dropped silently after this.
	alertStale = 24 * time.Hour
)

// healthWindows are the look-back periods of the rules.
type healthWindows struct {
	short  []store.AgentHealth // 10 minutes: the model
	medium []store.AgentHealth // 30 minutes: runs and tools
	hour   []store.AgentHealth // 60 minutes: tokens
}

func healthKey(h store.AgentHealth) string { return h.TenantID + "/" + h.Agent }

// evaluate returns the alerts that hold now, by tenant/agent/kind.
func evaluate(w healthWindows, spike int64, now time.Time) map[string]store.Alert {
	out := map[string]store.Alert{}
	add := func(h store.AgentHealth, kind, severity, summary string) {
		a := store.Alert{TenantID: h.TenantID, Agent: h.Agent, Kind: kind, Severity: severity, Summary: summary,
			RunID: h.LastFailedID, Since: now, LastSeen: now}
		out[alertKeyOf(a)] = a
	}
	model := map[string]bool{}
	for _, h := range w.short {
		switch {
		case h.AuthErrors > 0:
			add(h, AlertLLMAuth, "critical", fmt.Sprintf("The model provider rejected the API key (%d runs in 10 minutes). Last error: %s", h.AuthErrors, h.LastError))
			model[healthKey(h)] = true
		case h.ReachErrors >= 3:
			add(h, AlertLLMUnreachable, "warning", fmt.Sprintf("The model did not answer in %d runs within 10 minutes. Last error: %s", h.ReachErrors, h.LastError))
			model[healthKey(h)] = true
		}
	}
	for _, h := range w.medium {
		// A model outage fails every run; one mail about the cause is enough.
		if !model[healthKey(h)] && h.Failed >= 3 && h.Failed*2 > h.Runs {
			add(h, AlertRunsFailing, "warning", fmt.Sprintf("%d of %d runs failed in the last 30 minutes. Last error: %s", h.Failed, h.Runs, h.LastError))
		}
		if h.ToolErrors >= 5 && h.ToolErrors*2 > h.ToolCalls {
			add(h, AlertToolErrors, "warning", fmt.Sprintf("%d of %d tool calls failed in the last 30 minutes. The agent then answers without the data it needs.", h.ToolErrors, h.ToolCalls))
		}
	}
	for _, h := range w.hour {
		if h.Tokens > spike {
			add(h, AlertTokenSpike, "warning", fmt.Sprintf("%d tokens in the last hour, more than the limit of %d for an alert.", h.Tokens, spike))
		}
	}
	return out
}

func alertKeyOf(a store.Alert) string { return a.TenantID + "/" + a.Agent + "/" + a.Kind }

// checkAlerts runs one round of the checker. Called by the leader.
func (m *Manager) checkAlerts(ctx context.Context) {
	if m.st == nil || m.runLogRetention() == 0 {
		return
	}
	now := time.Now().UTC()
	var w healthWindows
	var err error
	if w.short, err = m.st.RunHealth(ctx, now.Add(-10*time.Minute)); err == nil {
		if w.medium, err = m.st.RunHealth(ctx, now.Add(-30*time.Minute)); err == nil {
			w.hour, err = m.st.RunHealth(ctx, now.Add(-time.Hour))
		}
	}
	if err != nil {
		m.log.Warn("alert check: reading run health failed", "err", err)
		return
	}
	spike := m.cfg.Tenancy.Notify.TokenSpikePerHour
	if spike <= 0 {
		spike = 2_000_000
	}
	current := evaluate(w, spike, now)
	stored, err := m.st.ListAlerts(ctx, "", true)
	if err != nil {
		m.log.Warn("alert check: listing alerts failed", "err", err)
		return
	}
	active := map[string]store.Alert{}
	for _, a := range stored {
		active[alertKeyOf(a)] = a
	}
	for k, a := range current {
		if a.TenantID == System {
			continue // no owner to tell
		}
		if old, ok := active[k]; ok {
			a.Since = old.Since
		} else if err := m.notify(ctx, notify.Event{Type: notify.TypeAlert, Tenant: a.TenantID, Agent: a.Agent, Kind: a.Kind,
			Severity: a.Severity, Subject: alertSubject(a), Text: a.Summary, RunID: a.RunID, At: now}); err != nil {
			// Not stored: the next round tries again.
			m.log.Warn("alert notification failed", "tenant", a.TenantID, "agent", a.Agent, "kind", a.Kind, "err", err)
			continue
		}
		if err := m.st.PutAlert(ctx, a); err != nil {
			m.log.Warn("storing alert failed", "err", err)
		}
	}
	// What no longer holds is resolved once the agent shows it works again;
	// an agent nobody uses keeps its alert until it goes stale.
	recovered := map[string]bool{}
	for _, h := range w.medium {
		if h.OK > 0 {
			recovered[healthKey(h)] = true
		}
	}
	for k, a := range active {
		if _, still := current[k]; still {
			continue
		}
		key := a.TenantID + "/" + a.Agent
		switch {
		case a.Kind == AlertTokenSpike || recovered[key]:
			if err := m.notify(ctx, notify.Event{Type: notify.TypeResolved, Tenant: a.TenantID, Agent: a.Agent, Kind: a.Kind,
				Severity: "info", Subject: "Resolved: " + alertSubject(a), Text: "The agent works normally again.", At: now}); err != nil {
				m.log.Warn("resolved notification failed", "tenant", a.TenantID, "agent", a.Agent, "err", err)
			}
		case now.Sub(a.LastSeen) < alertStale:
			continue
		}
		if err := m.st.DeleteAlert(ctx, a.TenantID, a.Agent, a.Kind); err != nil {
			m.log.Warn("deleting alert failed", "err", err)
		}
	}
}

func alertSubject(a store.Alert) string {
	switch a.Kind {
	case AlertRunsFailing:
		return "Agent " + a.Agent + " fails most of its runs"
	case AlertToolErrors:
		return "Tools of agent " + a.Agent + " keep failing"
	case AlertLLMAuth:
		return "The model rejects the API key of agent " + a.Agent
	case AlertLLMUnreachable:
		return "The model of agent " + a.Agent + " does not answer"
	case AlertTokenSpike:
		return "Agent " + a.Agent + " uses unusually many tokens"
	}
	return "Problem with agent " + a.Agent
}

// notify delivers an event; without a configured notifier it is a no-op
// (alerts are still stored and shown in the UI).
func (m *Manager) notify(ctx context.Context, e notify.Event) error {
	if m.notifier == nil {
		return nil
	}
	return m.notifier.Notify(ctx, e)
}

// Alerts lists active alerts of a tenant (all tenants for the admin).
func (m *Manager) Alerts(ctx context.Context, tenantID string, all bool) ([]store.Alert, error) {
	if m.st == nil {
		return []store.Alert{}, nil
	}
	return m.st.ListAlerts(ctx, tenantID, all)
}

// RunStats aggregates a tenant's run log.
func (m *Manager) RunStats(ctx context.Context, tenantID, agentName string, since time.Time) (*store.RunStats, error) {
	if m.st == nil {
		return &store.RunStats{Since: since, Days: []store.DayStats{}, Agents: []store.AgentStats{}}, nil
	}
	return m.st.RunStats(ctx, tenantID, agentName, since)
}
