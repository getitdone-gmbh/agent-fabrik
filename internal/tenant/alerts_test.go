package tenant

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/notify"
	"github.com/getitdone-gmbh/agent-fabrik/internal/store"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tools"
)

type captured struct {
	mu     sync.Mutex
	events []notify.Event
	fail   bool
}

func (c *captured) Notify(_ context.Context, e notify.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return context.DeadlineExceeded
	}
	c.events = append(c.events, e)
	return nil
}

func (c *captured) take() []notify.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

func TestEvaluate(t *testing.T) {
	now := time.Now()
	h := func(agent string, f func(*store.AgentHealth)) store.AgentHealth {
		x := store.AgentHealth{TenantID: "t1", Agent: agent}
		f(&x)
		return x
	}
	w := healthWindows{
		short: []store.AgentHealth{
			h("auth", func(x *store.AgentHealth) { x.Runs, x.Failed, x.AuthErrors = 1, 1, 1 }),
			h("down", func(x *store.AgentHealth) { x.Runs, x.Failed, x.ReachErrors = 3, 3, 3 }),
			h("blip", func(x *store.AgentHealth) { x.Runs, x.Failed, x.ReachErrors = 2, 2, 2 }),
		},
		medium: []store.AgentHealth{
			h("down", func(x *store.AgentHealth) { x.Runs, x.Failed = 5, 5 }), // cause already reported
			h("bad", func(x *store.AgentHealth) { x.Runs, x.Failed = 5, 3 }),
			h("mostly-ok", func(x *store.AgentHealth) { x.Runs, x.Failed = 10, 3 }),
			h("tools", func(x *store.AgentHealth) { x.Runs, x.OK, x.ToolCalls, x.ToolErrors = 4, 4, 8, 5 }),
			h("few-tools", func(x *store.AgentHealth) { x.Runs, x.ToolCalls, x.ToolErrors = 2, 4, 4 }),
		},
		hour: []store.AgentHealth{
			h("hungry", func(x *store.AgentHealth) { x.Tokens = 3_000_000 }),
			h("normal", func(x *store.AgentHealth) { x.Tokens = 100_000 }),
		},
	}
	got := map[string]bool{}
	for k := range evaluate(w, 2_000_000, now) {
		got[k] = true
	}
	want := []string{"t1/auth/llm_auth", "t1/down/llm_unreachable", "t1/bad/runs_failing", "t1/tools/tool_errors", "t1/hungry/token_spike"}
	for _, k := range want {
		if !got[k] {
			t.Errorf("missing %s", k)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want exactly %v", got, want)
	}
}

func TestAlertLifecycle(t *testing.T) {
	m, st := newManager(t, "")
	n := &captured{}
	m.SetNotifier(n)
	ctx := context.Background()
	now := time.Now().UTC()
	rec := func(id, status, kind string, at time.Time) {
		t.Helper()
		if err := st.RecordRun(ctx, store.Run{ID: id, TenantID: "t1", Agent: "support", Status: status, Error: "boom " + id, ErrorKind: kind, StartedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"r1", "r2", "r3"} {
		rec(id, store.RunError, "other", now.Add(time.Duration(-20+i)*time.Minute))
	}
	m.CheckAlerts(ctx)
	ev := n.take()
	if len(ev) != 1 || ev[0].Type != notify.TypeAlert || ev[0].Kind != AlertRunsFailing || ev[0].Tenant != "t1" || ev[0].RunID != "r3" ||
		!strings.Contains(ev[0].Text, "boom r3") {
		t.Fatalf("first round: %+v", ev)
	}
	alerts, _ := m.Alerts(ctx, "t1", false)
	if len(alerts) != 1 {
		t.Fatalf("stored: %+v", alerts)
	}
	since := alerts[0].Since

	m.CheckAlerts(ctx) // still failing: no second mail, Since kept
	if ev := n.take(); len(ev) != 0 {
		t.Errorf("notified twice: %+v", ev)
	}
	if alerts, _ = m.Alerts(ctx, "t1", false); !alerts[0].Since.Equal(since) {
		t.Errorf("since moved: %v -> %v", since, alerts[0].Since)
	}

	// The failures age out and the agent works again: resolved.
	st.PruneRuns(ctx, now.Add(time.Minute))
	rec("r4", store.RunOK, "", now)
	m.CheckAlerts(ctx)
	if ev := n.take(); len(ev) != 1 || ev[0].Type != notify.TypeResolved {
		t.Fatalf("resolve: %+v", ev)
	}
	if alerts, _ = m.Alerts(ctx, "t1", false); len(alerts) != 0 {
		t.Errorf("alert kept after resolve: %+v", alerts)
	}

	// An agent nobody uses keeps its alert until it is stale, then it goes
	// without a mail.
	st.PruneRuns(ctx, now.Add(time.Minute))
	st.PutAlert(ctx, store.Alert{TenantID: "t1", Agent: "idle", Kind: AlertRunsFailing, Since: now.Add(-30 * time.Hour), LastSeen: now.Add(-2 * time.Hour)})
	st.PutAlert(ctx, store.Alert{TenantID: "t1", Agent: "old", Kind: AlertRunsFailing, Since: now.Add(-30 * time.Hour), LastSeen: now.Add(-25 * time.Hour)})
	m.CheckAlerts(ctx)
	if ev := n.take(); len(ev) != 0 {
		t.Errorf("idle agents notified: %+v", ev)
	}
	alerts, _ = m.Alerts(ctx, "t1", false)
	if len(alerts) != 1 || alerts[0].Agent != "idle" {
		t.Errorf("stale handling: %+v", alerts)
	}
}

func TestAlertRetriedWhenNotifyFails(t *testing.T) {
	m, st := newManager(t, "")
	n := &captured{fail: true}
	m.SetNotifier(n)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, id := range []string{"a", "b", "c"} {
		st.RecordRun(ctx, store.Run{ID: id, TenantID: "t1", Agent: "x", Status: store.RunError, ErrorKind: "auth", StartedAt: now})
	}
	st.RecordRun(ctx, store.Run{ID: "s", TenantID: System, Agent: "x", Status: store.RunError, ErrorKind: "auth", StartedAt: now})
	m.CheckAlerts(ctx)
	if alerts, _ := m.Alerts(ctx, "", true); len(alerts) != 0 {
		t.Fatalf("alert stored although the owner was not told: %+v", alerts)
	}
	n.fail = false
	m.CheckAlerts(ctx)
	ev := n.take()
	if len(ev) != 1 || ev[0].Kind != AlertLLMAuth || ev[0].Severity != "critical" {
		t.Fatalf("retry: %+v", ev) // and nothing for the system tenant
	}
}

func TestNotifyTool(t *testing.T) {
	m, _ := newManager(t, "  notify: {messages_per_day: 2}\n")
	ctx := context.Background()
	if err := m.Apply(ctx, "t1", Mutation{Put: []store.Resource{res(store.KindProvider, "p", `{"type":"openai-compatible","base_url":"http://x"}`),
		res(store.KindAgent, "support", `{"provider":"p","tools":["notify.*"]}`)}}); err != nil {
		t.Fatal(err)
	}
	a, release, _ := m.Acquire(ctx, "t1")
	if _, ok := a.Registry.Get("notify__send"); ok {
		t.Error("notify tool offered without a notifier")
	}
	release()

	n := &captured{}
	m.SetNotifier(n)
	m.forgetTenantCache("t1")
	m.mu.Lock()
	delete(m.runtimes, "t1")
	m.mu.Unlock()
	a, release, err := m.Acquire(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	tool, ok := a.Registry.Get("notify__send")
	if !ok {
		t.Fatal("notify tool missing")
	}
	call := tools.WithCall(tools.WithCall(ctx, tools.Call{RunID: "run_1"}), tools.Call{Agent: "support"})
	args, _ := json.Marshal(map[string]string{"subject": "Rueckruf\n gewuenscht", "message": "Frau Meier bittet um Rueckruf.", "severity": "urgent"})
	if out, err := tool.Handler(call, args); err != nil || !strings.Contains(out, "sent") {
		t.Fatalf("send: %q %v", out, err)
	}
	ev := n.take()
	if len(ev) != 1 || ev[0].Type != notify.TypeMessage || ev[0].Tenant != "t1" || ev[0].Agent != "support" || ev[0].RunID != "run_1" ||
		ev[0].Subject != "Rueckruf gewuenscht" || ev[0].Severity != "critical" {
		t.Fatalf("event: %+v", ev)
	}
	if _, err := tool.Handler(call, json.RawMessage(`{"subject":"x","message":"y","severity":"loud"}`)); err == nil {
		t.Error("bad severity accepted")
	}
	tool.Handler(call, json.RawMessage(`{"subject":"x","message":"y"}`))
	if _, err := tool.Handler(call, json.RawMessage(`{"subject":"x","message":"y"}`)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("daily limit not enforced: %v", err)
	}
}
