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
