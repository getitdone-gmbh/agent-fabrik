package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/getitdone-gmbh/agentd/internal/testutil"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/agent"
	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/llm"
	"github.com/getitdone-gmbh/agentd/internal/store"
)

func newManager(t *testing.T, extra string) (*Manager, store.Store) {
	t.Helper()
	doc := `
tenancy:
  enabled: true
  runtime_idle_ttl: 50ms
  plans:
    default: {max_agents: 5, runs_per_minute: 2}
  default_plan: default
` + extra
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.NewMem("", time.Hour, 100)
	m, err := New(context.Background(), cfg, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CreateTenant(context.Background(), store.Tenant{ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	return m, st
}

func res(kind, name, spec string) store.Resource {
	return store.Resource{Kind: kind, Name: name, Spec: json.RawMessage(spec)}
}

func TestBoxBindsTenantAndName(t *testing.T) {
	b, _, _ := NewBox("")
	ct := b.Seal("t1", "KEY", []byte("v"))
	if pt, err := b.Open("t1", "KEY", ct); err != nil || string(pt) != "v" {
		t.Fatal(pt, err)
	}
	if _, err := b.Open("t2", "KEY", ct); err == nil {
		t.Error("ciphertext usable by another tenant")
	}
	if _, err := b.Open("t1", "OTHER", ct); err == nil {
		t.Error("ciphertext usable under another name")
	}
	if _, _, err := NewBox("short"); err == nil {
		t.Error("bad key accepted")
	}
}

func TestTenantNetworkIsGuarded(t *testing.T) {
	m, _ := newManager(t, "")
	ctx := context.Background()
	err := m.Apply(ctx, "t1", Mutation{Put: []store.Resource{
		res(store.KindProvider, "p", `{"type":"openai-compatible","base_url":"http://10.0.0.1:8080/v1","max_retries":0}`),
		res(store.KindConnector, "web", `{"type":"http","allow_hosts":["*"]}`),
		res(store.KindAgent, "a", `{"provider":"p","tools":["web.*"]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	a, release, err := m.Acquire(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	tool, ok := a.Registry.Get("web__request")
	if !ok {
		t.Fatal("tool missing")
	}
	for _, u := range []string{"http://127.0.0.1:1/", "http://169.254.169.254/latest/meta-data", "http://[::1]:1/"} {
		_, err := tool.Handler(ctx, json.RawMessage(`{"url":"`+u+`"}`))
		if err == nil || !strings.Contains(err.Error(), "internal address") {
			t.Errorf("%s: expected guard error, got %v", u, err)
		}
	}
	// Tenant provider pointing into the cluster network is blocked too.
	_, err = m.Run(ctx, "t1", "a", agent.RunInput{Input: "hi"})
	if err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Errorf("provider not guarded: %v", err)
	}
}

func TestRuntimeCacheLifecycle(t *testing.T) {
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer llmSrv.Close()
	m, _ := newManager(t, "  allow_networks: [127.0.0.1/32]\n")
	ctx := context.Background()
	put := func(sys string) {
		t.Helper()
		err := m.Apply(ctx, "t1", Mutation{Put: []store.Resource{
			res(store.KindProvider, "p", `{"type":"openai-compatible","base_url":"`+llmSrv.URL+`"}`),
			res(store.KindAgent, "a", `{"provider":"p","system":"`+sys+`"}`),
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("v1")
	a1, rel1, _ := m.Acquire(ctx, "t1")
	a2, rel2, _ := m.Acquire(ctx, "t1")
	if a1 != a2 {
		t.Error("runtime not shared")
	}
	rel2()
	put("v2") // definitions change -> new runtime; old one stays usable until released
	a3, rel3, _ := m.Acquire(ctx, "t1")
	if a3 == a1 || a3.Agents["a"].Config().System != "v2" {
		t.Error("runtime not rebuilt after change")
	}
	if a1.Agents["a"].Config().System != "v1" {
		t.Error("in-flight runtime mutated")
	}
	rel1()
	rel3()
	if n := m.Stats()["runtimes_loaded"]; n != 1 {
		t.Errorf("loaded runtimes %v", n)
	}
	time.Sleep(80 * time.Millisecond)
	m.sweepIdle()
	if n := m.Stats()["runtimes_loaded"]; n != 0 {
		t.Errorf("idle runtime not unloaded: %v", n)
	}
	// Rate limit: plan allows 2 runs per minute.
	for i := 0; i < 2; i++ {
		if _, err := m.Run(ctx, "t1", "a", agent.RunInput{Input: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Run(ctx, "t1", "a", agent.RunInput{Input: "x"}); !errors.Is(err, ErrRateLimited) {
		t.Errorf("want rate limit, got %v", err)
	}
}

func TestSystemTenantReadOnly(t *testing.T) {
	m, _ := newManager(t, "")
	if err := m.Apply(context.Background(), System, Mutation{}); !errors.Is(err, ErrReadOnly) {
		t.Error(err)
	}
	if _, err := m.CreateTenant(context.Background(), store.Tenant{ID: System}); err == nil {
		t.Error("system id accepted")
	}
	if _, err := m.CreateTenant(context.Background(), store.Tenant{ID: "Bad ID!"}); err == nil {
		t.Error("bad id accepted")
	}
}

// Two replicas on one database: only the leader schedules triggers, and
// changes made on the follower reach the leader; failover moves them.
func TestReplicasScheduleOnce(t *testing.T) {
	url := testutil.PostgresURL(t, "tenant")
	if url == "" {
		t.Skip("AR_TEST_DATABASE_URL not set")
	}
	cfg, err := config.Parse([]byte("tenancy:\n  enabled: true\n  store: {type: postgres, url: x}\n  secret_key: " + strings.Repeat("A", 43) + "=\n  plans: {p: {min_schedule_interval: 1s}}\n  default_plan: p\n"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mk := func() (*Manager, context.CancelFunc, *sync.WaitGroup, store.Store) {
		st, err := store.NewPostgres(context.Background(), url, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		m, err := New(ctx, cfg, st, nil, log)
		if err != nil {
			t.Fatal(err)
		}
		m.tick = 100 * time.Millisecond
		var wg sync.WaitGroup
		m.Start(ctx, &wg)
		return m, cancel, &wg, st
	}
	m1, cancel1, wg1, st1 := mk()
	m2, cancel2, wg2, st2 := mk()
	defer func() { cancel2(); wg2.Wait(); st2.Close() }()
	ctx := context.Background()
	id := "rep-" + time.Now().Format("150405.000000")
	id = strings.ReplaceAll(id, ".", "")
	if _, err := m2.CreateTenant(ctx, store.Tenant{ID: id}); err != nil {
		t.Fatal(err)
	}
	defer m2.DeleteTenant(ctx, id)
	err = m2.Apply(ctx, id, Mutation{Put: []store.Resource{
		res(store.KindProvider, "p", `{"type":"mistral","api_key":"x"}`),
		res(store.KindAgent, "a", `{"provider":"p","triggers":[{"type":"interval","every":"1h"}]}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	entries := func(m *Manager) int64 { return m.sched.Stats()["entries"] }
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if !cond() {
			t.Fatalf("timeout waiting for %s (m1=%d m2=%d)", what, entries(m1), entries(m2))
		}
	}
	waitFor("leader picks up follower change", func() bool { return entries(m1) >= 1 })
	if entries(m2) != 0 {
		t.Fatalf("follower schedules triggers too: %d", entries(m2))
	}
	cancel1()
	wg1.Wait()
	st1.Close() // leader dies -> advisory lock released
	waitFor("failover", func() bool { return entries(m2) >= 1 })
}

func TestExternalAuthOIDC(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer good":
			io.WriteString(w, `{"sub":"8f14e45f-ceea-467a-9575","email":"eve@example.com","org":{"plan":"pro"}}`)
		case "Bearer long":
			io.WriteString(w, `{"sub":"https://accounts.example.com/users/`+strings.Repeat("x", 80)+`"}`)
		default:
			w.WriteHeader(401)
		}
	}))
	defer idp.Close()
	m, st := newManager(t, "  external_auth:\n    enabled: true\n    userinfo_url: "+idp.URL+"\n    plan_field: org.plan\n    plan_map: {pro: default}\n")
	ctx := context.Background()
	id, err := m.Authenticate(ctx, "good")
	if err != nil || id != "ext-8f14e45f-ceea-467a-9575" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	tn, _ := st.GetTenant(ctx, id)
	if tn.Name != "eve@example.com" || tn.Plan != "default" {
		t.Errorf("tenant %+v", tn)
	}
	long, err := m.Authenticate(ctx, "long")
	if err != nil || len(long) > 63 || !strings.HasPrefix(long, "ext-") {
		t.Errorf("long subject: %q %v", long, err)
	}
	if again, _ := m.Authenticate(ctx, "long"); again != long {
		t.Error("long subject id not stable")
	}
	if _, err := m.Authenticate(ctx, "bad"); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("bad token: %v", err)
	}
}

func TestExternalIDsNeverCollide(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		io.WriteString(w, `{"email":"`+sub+`"}`)
	}))
	defer idp.Close()
	m, _ := newManager(t, "  external_auth:\n    enabled: true\n    userinfo_url: "+idp.URL+"\n    id_field: email\n")
	ctx := context.Background()
	a, err1 := m.Authenticate(ctx, "John.Doe@x.com")
	b, err2 := m.Authenticate(ctx, "johndoe@x.com")
	if err1 != nil || err2 != nil || a == b {
		t.Fatalf("a=%q b=%q err=%v %v", a, b, err1, err2)
	}
}

func TestExternalAuthRequireField(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer admin":
			io.WriteString(w, `{"id":1,"admin":true}`)
		case "Bearer user":
			io.WriteString(w, `{"id":2,"admin":false}`)
		case "Bearer missing":
			io.WriteString(w, `{"id":3}`)
		case "Bearer stringy":
			io.WriteString(w, `{"id":4,"admin":"yes"}`)
		default:
			w.WriteHeader(401)
		}
	}))
	defer idp.Close()
	m, st := newManager(t, "  external_auth:\n    enabled: true\n    userinfo_url: "+idp.URL+"\n    id_field: id\n    tenant_prefix: itd-\n    require_field: admin\n")
	ctx := context.Background()
	if id, err := m.Authenticate(ctx, "admin"); err != nil || id != "itd-1" {
		t.Fatalf("admin: id=%q err=%v", id, err)
	}
	for _, tok := range []string{"user", "missing", "stringy"} {
		if _, err := m.Authenticate(ctx, tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: want unauthorized, got %v", tok, err)
		}
		// Twice: the cached lookup must refuse as well.
		if _, err := m.Authenticate(ctx, tok); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s (cached): want unauthorized, got %v", tok, err)
		}
	}
	for _, id := range []string{"itd-2", "itd-3", "itd-4"} {
		if _, err := st.GetTenant(ctx, id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("tenant %s created for a refused user: %v", id, err)
		}
	}
}

func TestNegativeUsageIgnored(t *testing.T) {
	m, _ := newManager(t, "")
	ctx := context.Background()
	m.recordUsage("t1", llm.Usage{InputTokens: 100, OutputTokens: 10})
	m.recordUsage("t1", llm.Usage{InputTokens: -1_000_000, OutputTokens: -5})
	u, err := m.Usage(ctx, "t1")
	if err != nil || u.InputTokens != 100 || u.OutputTokens != 10 {
		t.Fatalf("usage %+v err=%v", u, err)
	}
}

// A tenant over its own concurrency cap must wait on its own slot, not on a
// process-wide one: before, five parallel runs of a one-slot tenant parked on
// five of the shared slots and a handful of tenants starved everyone else.
func TestTenantCapBeforeGlobalSlot(t *testing.T) {
	var inflight sync.WaitGroup
	block := make(chan struct{})
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Done()
		<-block
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer llmSrv.Close()
	cfg, err := config.Parse([]byte(`
server:
  max_concurrent_runs: 4
tenancy:
  enabled: true
  allow_networks: [127.0.0.1/32]
  plans:
    default: {max_agents: 5, runs_per_minute: 100, max_concurrent_runs: 1}
  default_plan: default
`))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.NewMem("", time.Hour, 100)
	m, err := New(context.Background(), cfg, st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := m.CreateTenant(ctx, store.Tenant{ID: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Apply(ctx, "t1", Mutation{Put: []store.Resource{
		res(store.KindProvider, "p", `{"type":"openai-compatible","base_url":"`+llmSrv.URL+`","max_retries":0}`),
		res(store.KindAgent, "a", `{"provider":"p"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	inflight.Add(1) // only one run may reach the model at a time
	var done sync.WaitGroup
	for i := 0; i < 5; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			if _, err := m.Run(ctx, "t1", "a", agent.RunInput{Input: "x"}); err != nil {
				t.Error(err)
			}
		}()
	}
	inflight.Wait()
	time.Sleep(50 * time.Millisecond) // let the other four queue up
	if n := m.Stats()["global_running"]; n != 1 {
		t.Errorf("shared slots held by one tenant: %v, want 1", n)
	}
	inflight.Add(4)
	close(block)
	done.Wait()
}

// Webhooks need no login and tenant ids are predictable: calls are counted
// per tenant and in total before any configuration is loaded.
func TestAllowHook(t *testing.T) {
	m, _ := newManager(t, "")
	n := 0
	for m.AllowHook("victim") {
		n++
	}
	if n != HooksPerTenantMinute {
		t.Errorf("one tenant got %d hook calls, want %d", n, HooksPerTenantMinute)
	}
	if !m.AllowHook("other") {
		t.Error("another tenant is limited by the first one's bucket")
	}
	rotating := 0
	for m.AllowHook(fmt.Sprintf("t%d", rotating)) {
		rotating++
	}
	if rotating >= HooksPerMinute {
		t.Errorf("rotating tenant ids got %d more calls, global cap is %d per minute", rotating, HooksPerMinute)
	}
}
