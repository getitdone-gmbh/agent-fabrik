package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/app"
	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/server"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tenant"
)

// newTenant creates a tenant on plan and gives it one agent on the shared
// provider; it returns the tenant's key.
func newTenant(t *testing.T, e *mtEnv, id, plan string) string {
	t.Helper()
	code, out := e.call(t, "admin-token", "POST", "/v1/admin/tenants", fmt.Sprintf(`{"id":%q,"plan":%q}`, id, plan))
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("create %s: %d %v", id, code, out)
	}
	key, _ := out["key"].(string)
	if code, out := e.call(t, key, "PUT", "/v1/config", "agents:\n  a: {provider: shared}\n"); code != http.StatusOK {
		t.Fatalf("config %s: %d %v", id, code, out)
	}
	return key
}

func TestDisabledTenantIsLockedOut(t *testing.T) {
	e := mtSetup(t, nil)
	key := newTenant(t, e, "dis", "free")
	if code, out := e.call(t, "admin-token", "PATCH", "/v1/admin/tenants/dis", `{"disabled":true}`); code != http.StatusOK {
		t.Fatalf("disable: %d %v", code, out)
	}
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/v1/files/x.txt", "hello"},
		{"POST", "/v1/keys", `{"name":"k"}`},
		{"PUT", "/v1/config", "agents: {}\n"},
		{"PUT", "/v1/secrets/S", `{"value":"v"}`},
		{"GET", "/v1/agents", ""},
		{"POST", "/v1/agents/a/run", `{"input":"hi"}`},
	} {
		if code, out := e.call(t, key, c.method, c.path, c.body); code != http.StatusForbidden {
			t.Errorf("%s %s by a disabled tenant: %d %v, want 403", c.method, c.path, code, out)
		}
	}
	// The admin can still look at it and switch it back on.
	req, _ := http.NewRequest("GET", e.url+"/v1/config", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Tenant", "dis")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("admin as disabled tenant: %v %v", resp.StatusCode, err)
	}
	e.call(t, "admin-token", "PATCH", "/v1/admin/tenants/dis", `{"disabled":false}`)
	time.Sleep(1100 * time.Millisecond) // tenant cache
	if code, out := e.call(t, key, "GET", "/v1/agents", ""); code != http.StatusOK {
		t.Errorf("re-enabled tenant: %d %v", code, out)
	}
}

func TestBudgetRecheckedAfterQueueing(t *testing.T) {
	e := mtSetup(t, nil)
	key := newTenant(t, e, "bud", "budget") // 20 tokens a month, one run at a time
	e.llm.delay.Store(int64(150 * time.Millisecond))
	// Every run costs 13 tokens. All five pass the check on arrival (0 used);
	// queued behind each other, only the first two may actually run.
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = e.call(t, key, "POST", "/v1/agents/a/run", `{"input":"hi"}`)
		}()
	}
	wg.Wait()
	ok, over := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			over++
		}
	}
	if ok > 2 || ok+over != len(codes) {
		t.Fatalf("runs over a 20-token budget: %v (%d ran, want at most 2)", codes, ok)
	}
}

func TestKeyRequestMustParse(t *testing.T) {
	e := mtSetup(t, nil)
	key := newTenant(t, e, "keys", "free")
	for _, body := range []string{`{"scope":123}`, `{"agents":"a"}`, `{`, `[]`} {
		if code, out := e.call(t, key, "POST", "/v1/keys", body); code != http.StatusBadRequest {
			t.Errorf("POST /v1/keys %s: %d %v, want 400", body, code, out)
		}
	}
	if code, out := e.call(t, key, "POST", "/v1/keys", ""); code != http.StatusCreated {
		t.Errorf("empty body (default key): %d %v", code, out)
	}
	if code, out := e.call(t, key, "POST", "/v1/keys", `{"scope":"run","agents":["a"]}`); code != http.StatusCreated {
		t.Errorf("run key: %d %v", code, out)
	}
}

// noTokenServer serves one agent without any token, as on a laptop.
func noTokenServer(t *testing.T) *httptest.Server {
	llm := fakeLLM(t)
	t.Cleanup(llm.Close)
	cfg, err := config.Parse([]byte(fmt.Sprintf("providers:\n  p: {type: openai-compatible, base_url: %s}\nagents:\n  a: {provider: p}\n", llm.URL)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.Build(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	m, err := tenant.New(ctx, cfg, nil, a, log)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(ctx, m, cfg))
	t.Cleanup(func() { srv.Close(); cancel(); a.Close() })
	return srv
}

func TestNoTokenOnlyLocalRequests(t *testing.T) {
	srv := noTokenServer(t)
	run := func(host, origin, site, ctype string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/agents/a/run", strings.NewReader(`{"input":"hi"}`))
		req.Header.Set("Content-Type", ctype)
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	for _, c := range []struct {
		name, host, origin, site, ctype string
		want                            int
	}{
		{"curl", "", "", "", "application/json", 200},
		{"localhost name", "localhost:" + port, "", "", "application/json", 200},
		{"own web UI", "", "http://127.0.0.1:" + port, "same-origin", "application/json", 200},
		{"foreign page, text/plain", "", "https://evil.example", "cross-site", "text/plain", 403},
		{"foreign page, no Sec-Fetch", "", "https://evil.example", "", "text/plain", 403},
		{"DNS rebinding", "evil.example:" + port, "http://evil.example:" + port, "same-origin", "text/plain", 403},
		{"null origin", "", "null", "", "text/plain", 403},
	} {
		if got := run(c.host, c.origin, c.site, c.ctype); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestSlowBodyIsCutOff(t *testing.T) {
	defer server.SetTimeouts(300*time.Millisecond, time.Second)()
	srv := noTokenServer(t)
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Announce a body and then send it a byte at a time, too slowly.
	fmt.Fprintf(conn, "POST /v1/agents/a/run HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{")
	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	go func() {
		for i := 0; i < 50; i++ {
			time.Sleep(100 * time.Millisecond)
			if _, err := conn.Write([]byte(" ")); err != nil {
				return
			}
		}
	}()
	b, _ := io.ReadAll(conn)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("slow body held the handler for %v (response %q)", waited, b)
	}
}

func TestLongRunOutlivesBodyDeadline(t *testing.T) {
	// The body deadline ends with the body: a run that takes longer than it
	// must still finish (a deadline left running would cancel its context).
	defer server.SetTimeouts(200*time.Millisecond, time.Second)()
	e := mtSetup(t, nil)
	key := newTenant(t, e, "slow", "pro")
	e.llm.delay.Store(int64(700 * time.Millisecond))
	if code, out := e.call(t, key, "POST", "/v1/agents/a/run", `{"input":"hi"}`); code != http.StatusOK {
		t.Fatalf("long run: %d %v", code, out)
	}
	// And the next request on the same keep-alive connection still works.
	if code, out := e.call(t, key, "POST", "/v1/agents/a/run", `{"input":"again"}`); code != http.StatusOK {
		t.Fatalf("second run on the connection: %d %v", code, out)
	}
}
