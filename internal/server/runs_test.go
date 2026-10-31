package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/server"
	"github.com/getitdone-gmbh/agent-fabrik/internal/store"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tenant"
	"github.com/getitdone-gmbh/agent-fabrik/internal/testutil"
)

func TestRunLogAPI(t *testing.T) {
	stores := map[string]func() store.Store{"mem": func() store.Store { return nil }}
	if url := testutil.PostgresURL(t, "server"); url != "" {
		stores["postgres"] = func() store.Store {
			p, err := store.NewPostgres(context.Background(), url, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) { runLogAPI(t, mk()) })
	}
}

func runLogAPI(t *testing.T, st store.Store) {
	e := mtSetup(t, st)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	acme, beta := "rl-acme"+suffix, "rl-beta"+suffix
	_, out := e.call(t, "admin-token", "POST", "/v1/admin/tenants", `{"id":"`+acme+`"}`)
	acmeKey := out["key"].(string)
	_, out = e.call(t, "admin-token", "POST", "/v1/admin/tenants", `{"id":"`+beta+`"}`)
	betaKey := out["key"].(string)

	doc := fmt.Sprintf(`
connectors:
  tools: {type: mcp, url: %s}
providers:
  dead: {type: openai-compatible, base_url: "http://127.0.0.1:1", max_retries: 0}
agents:
  helper: {provider: shared, tools: ["tools.*"]}
  broken: {provider: dead}
`, e.mcp)
	if code, out := e.call(t, acmeKey, "PUT", "/v1/config", doc); code != 200 {
		t.Fatalf("apply: %d %v", code, errMsg(out))
	}
	if code, out := e.call(t, acmeKey, "POST", "/v1/agents/helper/run", `{"input":"hi from acme"}`); code != 200 {
		t.Fatalf("run: %d %v", code, out)
	}
	if code, _ := e.call(t, acmeKey, "POST", "/v1/agents/broken/run", `{"input":"will fail"}`); code < 400 {
		t.Fatalf("broken agent answered %d", code)
	}

	code, out := e.call(t, acmeKey, "GET", "/v1/runs", "")
	runs, _ := out["runs"].([]any)
	if code != 200 || len(runs) != 2 {
		t.Fatalf("list: %d %v", code, out)
	}
	failed, ok := runs[0].(map[string]any), runs[1].(map[string]any)
	if failed["agent"] != "broken" || failed["status"] != "error" || failed["error"] == "" {
		t.Errorf("failed run: %v", failed)
	}
	if ok["agent"] != "helper" || ok["status"] != "ok" || ok["source"] != "api" || ok["input"] != "hi from acme" ||
		ok["tool_calls"].(float64) != 1 || ok["trace"] != nil {
		t.Errorf("ok run: %v", ok)
	}
	id := ok["id"].(string)

	code, out = e.call(t, acmeKey, "GET", "/v1/runs/"+id, "")
	trace, _ := out["trace"].([]any)
	if code != 200 || len(trace) < 2 || !strings.Contains(fmt.Sprint(trace[0]), "echo") || out["output"] != "ANSWER: echo: hi from acme" {
		t.Fatalf("detail: %d %v", code, out)
	}

	if _, out := e.call(t, acmeKey, "GET", "/v1/runs?status=failed", ""); len(out["runs"].([]any)) != 1 {
		t.Errorf("failed filter: %v", out)
	}
	if code, _ := e.call(t, acmeKey, "GET", "/v1/runs?status=bogus", ""); code != 400 {
		t.Errorf("bad status: %d", code)
	}

	// Isolation: another tenant sees nothing, a run key (what a website
	// backend holds) cannot read the conversations, only the admin sees all.
	if code, _ := e.call(t, betaKey, "GET", "/v1/runs/"+id, ""); code != 404 {
		t.Errorf("other tenant read the run: %d", code)
	}
	if _, out := e.call(t, betaKey, "GET", "/v1/runs", ""); len(out["runs"].([]any)) != 0 {
		t.Errorf("other tenant lists runs: %v", out)
	}
	if code, _ := e.call(t, acmeKey, "GET", "/v1/runs?tenant=*", ""); code != 403 {
		t.Errorf("tenant used the admin overview: %d", code)
	}
	_, out = e.call(t, acmeKey, "POST", "/v1/keys", `{"name":"site","scope":"run"}`)
	runKey, _ := out["key"].(string)
	if code, _ := e.call(t, runKey, "GET", "/v1/runs", ""); code != 403 {
		t.Errorf("run key read the run log: %d", code)
	}
	if code, out := e.call(t, "admin-token", "GET", "/v1/runs?tenant=*", ""); code != 200 || !strings.Contains(fmt.Sprint(out), id) {
		t.Errorf("admin overview: %d %v", code, out)
	}
	if code, _ := e.call(t, "admin-token", "GET", "/v1/runs/"+id+"?tenant="+acme, ""); code != 200 {
		t.Errorf("admin detail: %d", code)
	}

	resp, err := http.Get(e.url + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`agent_runs_total{tenant="` + acme + `",agent="helper",source="api",status="ok"} 1`,
		`agent_runs_total{tenant="` + acme + `",agent="broken",source="api",status="error"} 1`,
		`agent_llm_errors_total{provider="dead",kind="network"}`,
		`agent_run_duration_seconds_count{tenant="` + acme + `",agent="helper"} 1`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	if !strings.Contains(string(b), `agent_tool_results_total{tenant="`+acme+`",agent="helper",tool="`) {
		t.Error("metrics missing tool results")
	}
}

// With metrics_addr the API port must not serve /metrics: behind the public
// ingress it would list tenants and agent names.
func TestMetricsOffTheAPIPort(t *testing.T) {
	cfg, err := config.Parse([]byte("server:\n  auth_token: x\n  metrics_addr: 127.0.0.1:0\nagents:\n  a: {}\nproviders:\n  p: {type: openai-compatible, base_url: http://127.0.0.1:1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := tenant.New(context.Background(), cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(context.Background(), m, cfg))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("API port still serves /metrics")
	}
	rec := httptest.NewRecorder()
	server.MetricsHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "agent_runs_total") {
		t.Error("metrics handler empty")
	}
}
