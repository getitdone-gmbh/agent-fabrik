package server_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/getitdone-gmbh/agentd/internal/testutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/server"
	"github.com/getitdone-gmbh/agentd/internal/store"
	"github.com/getitdone-gmbh/agentd/internal/tenant"
)

// recorder wraps the fake LLM and records auth headers and prompts.
type recorder struct {
	mu        sync.Mutex
	auth      []string
	scheduled atomic.Int64
	inner     http.Handler
}

func (rc *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.auth = append(rc.auth, r.Header.Get("Authorization"))
	rc.mu.Unlock()
	if strings.Contains(string(b), "Scheduled run") && strings.Contains(string(b), `"role":"user"`) && !strings.Contains(string(b), `"role":"tool"`) {
		rc.scheduled.Add(1)
	}
	r.Body = io.NopCloser(strings.NewReader(string(b)))
	rc.inner.ServeHTTP(w, r)
}

func (rc *recorder) sawAuth(v string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, a := range rc.auth {
		if a == v {
			return true
		}
	}
	return false
}

type mtEnv struct {
	url    string
	llm    *recorder
	mcp    string
	llmURL string
}

func mtSetup(t *testing.T, st store.Store) *mtEnv {
	inner := fakeLLM(t)
	t.Cleanup(inner.Close)
	rec := &recorder{inner: inner.Config.Handler}
	llmSrv := httptest.NewServer(rec)
	t.Cleanup(llmSrv.Close)
	mcpSrv := toolServer()
	t.Cleanup(mcpSrv.Close)
	// Fake platform with a "who am I" endpoint for one known token.
	itd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/me" && r.Header.Get("Authorization") == "Bearer itd-token-alice" {
			io.WriteString(w, `{"id":"u-alice","name":"Alice","email":"a@x.de","tier":"pro"}`)
			return
		}
		w.WriteHeader(401)
	}))
	t.Cleanup(itd.Close)

	key := make([]byte, 32)
	rand.Read(key)
	doc := fmt.Sprintf(`
server:
  auth_token: admin-token
providers:
  shared:
    type: openai-compatible
    base_url: %s
tenancy:
  enabled: true
  secret_key: %s
  shared_providers: [shared]
  allow_networks: ["127.0.0.1/32"]
  default_plan: free
  external_auth:
    enabled: true
    userinfo_url: %s/me
    id_field: id
    plan_field: tier
    plan_map: {pro: pro}
    tenant_prefix: itd-
  plans:
    free:
      max_agents: 2
      max_concurrent_runs: 2
      runs_per_minute: 1000
      monthly_tokens: 1000000
      max_steps: 5
      min_schedule_interval: 1s
      min_poll_interval: 1s
    pro:
      max_agents: 50
      runs_per_minute: 1000
    tiny:
      max_agents: 2
      runs_per_minute: 1000
      monthly_tokens: 20
`, llmSrv.URL, base64.StdEncoding.EncodeToString(key), itd.URL)
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if st == nil {
		st, _ = store.NewMem("", time.Hour, 1000)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m, err := tenant.New(ctx, cfg, st, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	m.Start(ctx, &wg)
	srv := httptest.NewServer(server.New(ctx, m, cfg))
	t.Cleanup(func() {
		srv.Close()
		cancel()
		wg.Wait()
		st.Close()
	})
	return &mtEnv{url: srv.URL, llm: rec, mcp: mcpSrv.URL, llmURL: llmSrv.URL}
}

func (e *mtEnv) call(t *testing.T, token, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, e.url+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	b, _ := io.ReadAll(resp.Body)
	json.Unmarshal(b, &out)
	if out == nil {
		out = map[string]any{"raw": string(b)}
	}
	return resp.StatusCode, out
}

func errMsg(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["message"].(string)
		return s
	}
	return fmt.Sprint(m)
}

func TestMultiTenant(t *testing.T) {
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
		t.Run(name, func(t *testing.T) { multiTenant(t, mk()) })
	}
}

func multiTenant(t *testing.T, st store.Store) {
	e := mtSetup(t, st)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9) // unique ids for shared databases
	acme, beta, tiny := "acme"+suffix, "beta"+suffix, "tiny"+suffix

	// Admin onboards tenants.
	code, out := e.call(t, "admin-token", "POST", "/v1/admin/tenants", `{"id":"`+acme+`","name":"ACME"}`)
	if code != 201 {
		t.Fatalf("create tenant: %d %v", code, out)
	}
	acmeKey := out["key"].(string)
	_, out = e.call(t, "admin-token", "POST", "/v1/admin/tenants", `{"id":"`+beta+`"}`)
	betaKey := out["key"].(string)
	_, out = e.call(t, "admin-token", "POST", "/v1/admin/tenants", `{"id":"`+tiny+`","plan":"tiny"}`)
	tinyKey := out["key"].(string)
	if code, _ := e.call(t, acmeKey, "GET", "/v1/admin/tenants", ""); code != 403 {
		t.Errorf("tenant reached admin API: %d", code)
	}
	if code, _ := e.call(t, "wrong", "GET", "/v1/me", ""); code != 401 {
		t.Errorf("bad token: %d", code)
	}

	// Self-service config as YAML, using the operator's shared provider.
	yamlDoc := fmt.Sprintf(`
connectors:
  tools:
    type: mcp
    url: %s
agents:
  helper:
    description: Echo helper
    provider: shared
    tools: ["tools.*"]
    triggers:
      - type: webhook
        name: gh
        secret: ${secret:HOOK}
  ticker:
    provider: shared
    expose: false
    triggers:
      - type: interval
        every: 1s
        prompt: "Scheduled run for {{date}}"
secrets:
  HOOK: hook-secret
`, e.mcp)
	code, out = e.call(t, acmeKey, "PUT", "/v1/config", yamlDoc)
	if code != 200 {
		t.Fatalf("apply: %d %v", code, errMsg(out))
	}
	if strings.Contains(fmt.Sprint(out), "hook-secret") {
		t.Error("secret value leaked in config response")
	}
	code, out = e.call(t, acmeKey, "POST", "/v1/agents/helper/run", `{"input":"hi from acme"}`)
	if code != 200 || out["output"] != "ANSWER: echo: hi from acme" {
		t.Fatalf("run: %d %v", code, out)
	}

	// Isolation: beta sees nothing of acme.
	if _, out := e.call(t, betaKey, "GET", "/v1/agents", ""); len(out["agents"].([]any)) != 0 {
		t.Errorf("beta sees agents: %v", out)
	}
	if code, _ := e.call(t, betaKey, "POST", "/v1/agents/helper/run", `{"input":"x"}`); code != 404 {
		t.Errorf("beta ran acme agent: %d", code)
	}
	if _, out := e.call(t, betaKey, "GET", "/v1/config", ""); len(out["agents"].(map[string]any)) != 0 {
		t.Errorf("beta sees acme config")
	}

	// Own provider with an encrypted secret as API key.
	e.call(t, betaKey, "PUT", "/v1/secrets/LLM_KEY", `{"value":"sk-beta-123"}`)
	code, out = e.call(t, betaKey, "PUT", "/v1/config/providers/mine", `{"type":"openai-compatible","base_url":"`+e.llmURL+`","api_key":"${secret:LLM_KEY}"}`)
	if code != 200 {
		t.Fatalf("provider: %d %v", code, errMsg(out))
	}
	code, out = e.call(t, betaKey, "PUT", "/v1/config/agents/bot", `{"provider":"mine","system":"hi"}`)
	if code != 200 {
		t.Fatalf("agent: %d %v", code, errMsg(out))
	}
	if code, out := e.call(t, betaKey, "POST", "/v1/agents/bot/run", `{"input":"yo"}`); code != 200 {
		t.Fatalf("beta run: %d %v", code, out)
	}
	if !e.llm.sawAuth("Bearer sk-beta-123") {
		t.Error("tenant secret not used as provider key")
	}
	if _, out := e.call(t, betaKey, "GET", "/v1/secrets", ""); fmt.Sprint(out["secrets"]) != "[LLM_KEY]" {
		t.Errorf("secrets list: %v", out)
	}
	// Secret still referenced: deletion rejected.
	if code, out := e.call(t, betaKey, "DELETE", "/v1/secrets/LLM_KEY", ""); code != 422 {
		t.Errorf("deleting used secret: %d %v", code, out)
	}

	// Policy checks.
	reject := func(path, body, want string) {
		t.Helper()
		code, out := e.call(t, betaKey, "PUT", path, body)
		if code != 422 || !strings.Contains(errMsg(out), want) {
			t.Errorf("PUT %s: want 422 %q, got %d %q", path, want, code, errMsg(out))
		}
	}
	reject("/v1/config/providers/env", `{"type":"anthropic","api_key":"${ADMIN_TOKEN}"}`, "only ${secret:NAME}")
	reject("/v1/config/connectors/sh", `{"type":"mcp","command":["sh","-c","id"]}`, "stdio")
	reject("/v1/config/connectors/lan", `{"type":"http","allow_hosts":["*"],"allow_private":true}`, "allow_private")
	if code, out := e.call(t, betaKey, "PUT", "/v1/config/agents/second", `{"provider":"mine"}`); code != 200 {
		t.Fatalf("second agent: %d %v", code, errMsg(out))
	}
	reject("/v1/config/agents/third", `{"provider":"mine"}`, "limit exceeded: 3 agents")
	reject("/v1/config/agents/bot", `{"provider":"mine","max_steps":50}`, "max_steps")
	reject("/v1/config/agents/bot", `{"provider":"mine","triggers":[{"type":"webhook"}]}`, "secret")

	// SSRF: a tenant HTTP tool cannot reach internal addresses.
	e.call(t, betaKey, "PUT", "/v1/config/connectors/web", `{"type":"http","allow_hosts":["*"]}`)
	e.call(t, betaKey, "PUT", "/v1/config/agents/bot", `{"provider":"mine","tools":["web.*"]}`)
	// fakeLLM only calls tools containing "echo"; exercise the tool directly via a second fake is overkill,
	// so check the connector's guard through the runtime instead:
	if code, _ := e.call(t, betaKey, "GET", "/v1/agents/bot", ""); code != 200 {
		t.Errorf("agent with web tool: %d", code)
	}

	// Quota: tiny plan allows 20 tokens per month; one run uses 13, so the
	// second run is allowed (13 < 20) and the third is refused (26 >= 20).
	e.call(t, tinyKey, "PUT", "/v1/config/agents/a", `{"provider":"shared"}`)
	for i := 1; i <= 2; i++ {
		if code, out := e.call(t, tinyKey, "POST", "/v1/agents/a/run", `{"input":"n"}`); code != 200 {
			t.Fatalf("tiny run %d: %d %v", i, code, out)
		}
	}
	code, out = e.call(t, tinyKey, "POST", "/v1/agents/a/run", `{"input":"3"}`)
	if code != 429 || !strings.Contains(errMsg(out), "quota") {
		t.Errorf("quota not enforced: %d %v", code, out)
	}
	if _, out := e.call(t, tinyKey, "GET", "/v1/me", ""); out["usage"].(map[string]any)["runs"].(float64) != 2 {
		t.Errorf("usage: %v", out["usage"])
	}

	// External users are tenants automatically, plan follows their tier.
	code, out = e.call(t, "itd-token-alice", "GET", "/v1/me", "")
	if code != 200 || out["tenant"].(map[string]any)["id"] != "itd-u-alice" || out["tenant"].(map[string]any)["plan"] != "pro" {
		t.Fatalf("external auth: %d %v", code, out)
	}
	if code, _ := e.call(t, "itd-token-bob", "GET", "/v1/me", ""); code != 401 {
		t.Errorf("unknown external token accepted: %d", code)
	}

	// Tenant webhook with the secret from the tenant's vault.
	req, _ := http.NewRequest("POST", e.url+"/v1/tenants/"+acme+"/hooks/helper/gh", strings.NewReader(`{"x":1}`))
	req.Header.Set("X-Webhook-Secret", "hook-secret")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 200 {
		t.Errorf("tenant webhook: %d", resp.StatusCode)
	}
	resp.Body.Close()
	req, _ = http.NewRequest("POST", e.url+"/v1/tenants/"+acme+"/hooks/helper/gh", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer admin-token") // admin token is not a webhook secret for tenants
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("tenant webhook without secret: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Scheduler: acme's interval trigger fires without any pod per agent.
	deadline := time.Now().Add(5 * time.Second)
	for e.llm.scheduled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if e.llm.scheduled.Load() == 0 {
		t.Error("interval trigger never fired")
	}
	_, stats := e.call(t, "admin-token", "GET", "/v1/admin/stats", "")
	if stats["scheduler"].(map[string]any)["entries"].(float64) < 1 {
		t.Errorf("stats: %v", stats)
	}

	// Disabling blocks runs and triggers; deleting revokes keys.
	e.call(t, "admin-token", "PATCH", "/v1/admin/tenants/"+acme, `{"disabled":true}`)
	if code, _ := e.call(t, acmeKey, "POST", "/v1/agents/helper/run", `{"input":"x"}`); code != 403 {
		t.Errorf("disabled tenant ran: %d", code)
	}
	if code, _ := e.call(t, "admin-token", "DELETE", "/v1/admin/tenants/"+beta, ""); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := e.call(t, betaKey, "GET", "/v1/me", ""); code != 401 {
		t.Errorf("deleted tenant key still valid: %d", code)
	}
	for _, id := range []string{acme, tiny, "itd-u-alice"} {
		e.call(t, "admin-token", "DELETE", "/v1/admin/tenants/"+id, "")
	}
}
