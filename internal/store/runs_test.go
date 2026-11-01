package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestRunLog(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := mk(t)
			defer s.Close()
			ctx := context.Background()
			if err := s.CreateTenant(ctx, Tenant{ID: "t1"}); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			add := func(id, tenant, agent, status string, at time.Time) {
				t.Helper()
				r := Run{ID: id, TenantID: tenant, Agent: agent, Source: "api", Status: status, Input: "frage " + id + "\x00",
					Output: strings.Repeat("a", 500), StartedAt: at,
					Trace: []TraceStep{{Type: "tool_call", Tool: "web", Args: json.RawMessage(`{"q":"x"}`)}, {Type: "tool_result", Tool: "web", Text: "nul\x00", IsError: true}}}
				if err := s.RecordRun(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			add("r1", "t1", "support", RunOK, now.Add(-3*time.Hour))
			add("r2", "t1", "support", RunError, now.Add(-2*time.Hour))
			add("r3", "t1", "sales", RunMaxSteps, now.Add(-time.Hour))
			add("r4", "system", "support", RunOK, now.Add(-40*24*time.Hour)) // no tenant row: still recorded

			ids := func(f RunFilter, tenant string) string {
				t.Helper()
				rs, err := s.ListRuns(ctx, tenant, f)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, r := range rs {
					if r.Trace != nil || len(r.Output) > previewLen+len("…") {
						t.Errorf("listing not shortened: %+v", r)
					}
					out = append(out, r.ID)
				}
				return strings.Join(out, ",")
			}
			for want, f := range map[string]RunFilter{
				"r3,r2,r1": {},
				"r2,r1":    {Agent: "support"},
				"r3,r2":    {Status: "failed"},
				"r2":       {Status: RunError},
				"r1":       {Before: now.Add(-2 * time.Hour)},
				"r3":       {Limit: 1},
			} {
				if got := ids(f, "t1"); got != want {
					t.Errorf("%+v: got %s, want %s", f, got, want)
				}
			}
			if got := ids(RunFilter{AllTenants: true}, ""); got != "r3,r2,r1,r4" {
				t.Errorf("all tenants: %s", got)
			}

			r, err := s.GetRun(ctx, "t1", "r2")
			if err != nil || len(r.Trace) != 2 || r.Trace[1].Text != "nul" || r.Input != "frage r2" || !r.Trace[1].IsError {
				t.Fatalf("get: %+v %v", r, err)
			}
			if _, err := s.GetRun(ctx, "other", "r2"); !errors.Is(err, ErrNotFound) {
				t.Errorf("run visible to another tenant: %v", err)
			}

			if n, err := s.PruneRuns(ctx, now.Add(-30*24*time.Hour)); err != nil || n != 1 {
				t.Errorf("prune: %d %v", n, err)
			}
			if err := s.DeleteTenant(ctx, "t1"); err != nil {
				t.Fatal(err)
			}
			if got := ids(RunFilter{AllTenants: true}, ""); got != "" {
				t.Errorf("runs survived tenant deletion: %s", got)
			}
		})
	}
}

func TestRunBound(t *testing.T) {
	r := Run{Input: strings.Repeat("ä", maxRunText), Trace: make([]TraceStep, maxTraceSteps+5)}
	r.Trace[0] = TraceStep{Args: json.RawMessage(`{"x":"\u0000"}`)}
	b := r.Bound()
	if len(b.Input) > maxRunText+len("…") || !strings.HasSuffix(b.Input, "…") || !json.Valid([]byte(`"`+b.Input+`"`)) {
		t.Errorf("input not clipped on a rune boundary: %d", len(b.Input))
	}
	if len(b.Trace) != maxTraceSteps+1 || b.Trace[maxTraceSteps].Type != "note" {
		t.Errorf("trace not capped: %d", len(b.Trace))
	}
	// jsonb rejects a \u0000 escape; the args are kept as a plain string.
	var v any
	if err := json.Unmarshal(b.Trace[0].Args, &v); err != nil || strings.Contains(fmt.Sprint(v), "\x00") {
		t.Errorf("NUL kept in args: %s", b.Trace[0].Args)
	}
	if len(r.Trace[0].Args) == 0 {
		t.Error("Bound modified its input")
	}
}

func TestRunStatsAndAlerts(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := mk(t)
			defer s.Close()
			ctx := context.Background()
			s.CreateTenant(ctx, Tenant{ID: "t1"})
			day := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
			for i, r := range []Run{
				{Agent: "a", Status: RunOK, DurationMS: 1000, InputTokens: 10, OutputTokens: 5, ToolCalls: 2, StartedAt: day},
				{Agent: "a", Status: RunError, ErrorKind: "auth", DurationMS: 3000, StartedAt: day.Add(time.Hour)},
				{Agent: "b", Status: RunMaxSteps, DurationMS: 2000, ToolCalls: 3, ToolErrors: 2, StartedAt: day.Add(24 * time.Hour)},
				{Agent: "b", Status: RunOK, DurationMS: 500, StartedAt: day.Add(-48 * time.Hour)}, // before since
			} {
				r.ID, r.TenantID = fmt.Sprint("r", i), "t1"
				if err := s.RecordRun(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			st, err := s.RunStats(ctx, "t1", "", day.Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if st.Totals.Runs != 3 || st.Totals.OK != 1 || st.Totals.Failed != 2 || st.Totals.ToolErrors != 2 || st.Totals.AvgMS != 2000 || st.Totals.P95MS != 3000 {
				t.Errorf("totals: %+v", st.Totals)
			}
			if len(st.Days) != 2 || st.Days[0].Date != "2026-10-30" || st.Days[0].Runs != 2 || st.Days[1].Runs != 1 {
				t.Errorf("days: %+v", st.Days)
			}
			if len(st.Agents) != 2 || st.Agents[0].Agent != "a" || st.Agents[1].Failed != 1 {
				t.Errorf("agents: %+v", st.Agents)
			}
			if st, _ := s.RunStats(ctx, "t1", "b", day.Add(-time.Hour)); st.Totals.Runs != 1 {
				t.Errorf("agent filter: %+v", st.Totals)
			}
			if st, _ := s.RunStats(ctx, "other", "", day.Add(-time.Hour)); st.Totals.Runs != 0 || st.Days == nil {
				t.Errorf("other tenant: %+v", st)
			}

			hs, err := s.RunHealth(ctx, day.Add(-time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hs {
				if h.Agent == "a" && (h.Runs != 2 || h.AuthErrors != 1 || h.LastFailedID != "r1") {
					t.Errorf("health a: %+v", h)
				}
			}

			a := Alert{TenantID: "t1", Agent: "a", Kind: "llm_auth", Severity: "critical", Summary: "x", RunID: "r1", Since: day, LastSeen: day}
			s.PutAlert(ctx, a)
			a.Summary, a.LastSeen, a.Since = "y", day.Add(time.Hour), day.Add(time.Hour) // since must stay
			s.PutAlert(ctx, a)
			l, _ := s.ListAlerts(ctx, "t1", false)
			if len(l) != 1 || l[0].Summary != "y" || !l[0].Since.Equal(day) {
				t.Errorf("alerts: %+v", l)
			}
			if l, _ := s.ListAlerts(ctx, "other", false); len(l) != 0 {
				t.Errorf("alert visible to another tenant")
			}
			s.DeleteAlert(ctx, "t1", "a", "llm_auth")
			if l, _ := s.ListAlerts(ctx, "", true); len(l) != 0 {
				t.Errorf("delete: %+v", l)
			}
		})
	}
}

func TestSessions(t *testing.T) {
	for name, mk := range backends(t) {
		t.Run(name, func(t *testing.T) {
			s := mk(t)
			defer s.Close()
			ctx := context.Background()
			s.CreateTenant(ctx, Tenant{ID: "t1"})
			t0 := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
			long := strings.Repeat("x", 1000)
			for i, r := range []Run{
				{SessionID: "s_a", Agent: "support", Status: RunOK, Input: "Hallo", Output: long, StartedAt: t0},
				{SessionID: "s_a", Agent: "support", Status: RunError, Input: "Und die Preise?", StartedAt: t0.Add(2 * time.Minute)},
				{SessionID: "s_b", Agent: "support", Status: RunOK, Input: "Oeffnungszeiten?", StartedAt: t0.Add(time.Hour)},
				{SessionID: "s_c", Agent: "sales", Status: RunOK, Input: "Angebot", StartedAt: t0.Add(30 * time.Minute)},
				{Agent: "support", Status: RunOK, Input: "ohne Session", StartedAt: t0},
			} {
				r.ID, r.TenantID = fmt.Sprint("r", i), "t1"
				s.RecordRun(ctx, r)
			}
			ss, err := s.ListSessions(ctx, "t1", SessionFilter{Agent: "support"})
			if err != nil {
				t.Fatal(err)
			}
			if len(ss) != 2 || ss[0].SessionID != "s_b" || ss[1].SessionID != "s_a" || ss[1].Runs != 2 || ss[1].Failed != 1 ||
				ss[1].FirstInput != "Hallo" || !ss[1].LastAt.Equal(t0.Add(2*time.Minute)) {
				t.Errorf("sessions: %+v", ss)
			}
			if ss, _ := s.ListSessions(ctx, "t1", SessionFilter{Failed: true}); len(ss) != 1 || ss[0].SessionID != "s_a" {
				t.Errorf("failed only: %+v", ss)
			}
			if ss, _ := s.ListSessions(ctx, "t1", SessionFilter{Before: t0.Add(time.Hour)}); len(ss) != 2 || ss[0].SessionID != "s_c" {
				t.Errorf("paging: %+v", ss)
			}
			if ss, _ := s.ListSessions(ctx, "other", SessionFilter{}); len(ss) != 0 {
				t.Errorf("other tenant: %+v", ss)
			}
			runs, _ := s.ListRuns(ctx, "t1", RunFilter{SessionID: "s_a"})
			if len(runs) != 2 || runs[1].Output != long || runs[0].Input != "Und die Preise?" {
				t.Errorf("conversation runs must be complete: %d runs", len(runs))
			}
		})
	}
}

func TestRunTotalsBreakdown(t *testing.T) {
	now := time.Now()
	st := statsOf([]Run{
		{Agent: "a", Status: RunOK, StartedAt: now},
		{Agent: "a", Status: RunOK, ToolCalls: 2, ToolErrors: 1, StartedAt: now},
		{Agent: "a", Status: RunMaxSteps, StartedAt: now},
		{Agent: "a", Status: RunError, ErrorKind: "timeout", StartedAt: now},
		{Agent: "a", Status: RunError, ErrorKind: "throttled", StartedAt: now},
		{Agent: "a", Status: RunError, ErrorKind: "client", StartedAt: now},
	}, now.Add(-time.Hour))
	got := st.Totals
	if got.OK != 2 || got.Clean != 1 || got.OKToolErrors != 1 || got.Failed != 4 || got.MaxSteps != 1 || got.ModelErrors != 2 || got.OtherErrors != 1 {
		t.Errorf("breakdown: %+v", got)
	}
	if got.OK != got.Clean+got.OKToolErrors || got.Failed != got.MaxSteps+got.ModelErrors+got.OtherErrors {
		t.Errorf("parts do not add up: %+v", got)
	}
}
