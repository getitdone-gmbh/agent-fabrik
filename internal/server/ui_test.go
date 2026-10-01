package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/server"
	"github.com/getitdone-gmbh/agentd/internal/tenant"
)

func TestUI(t *testing.T) {
	e := setup(t)
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := noRedirect.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("/ = %d %q, want redirect to /ui/", resp.StatusCode, resp.Header.Get("Location"))
	}

	// The page is public (it holds no data); the API behind it is not.
	for path, want := range map[string]string{"/ui/": "<title>agentd</title>", "/ui/app.js": "streamRun", "/ui/app.css": "--accent"} {
		resp, body := e.do(t, "GET", path, "", nil)
		if resp.StatusCode != 200 || !strings.Contains(body, want) {
			t.Fatalf("%s = %d, missing %q", path, resp.StatusCode, want)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Fatalf("%s: CSP = %q", path, csp)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s: missing nosniff", path)
		}
	}
	if resp, _ := e.do(t, "GET", "/ui/app.js", "", nil); !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("app.js content type %q", resp.Header.Get("Content-Type"))
	}
	if resp, _ := e.do(t, "GET", "/ui/nope.js", "", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown file = %d", resp.StatusCode)
	}
	if resp, _ := e.do(t, "GET", "/v1/agents", "", nil); resp.StatusCode != 401 {
		t.Fatalf("API without token = %d", resp.StatusCode)
	}
}

func TestUIDisabled(t *testing.T) {
	cfg, err := config.Parse([]byte("server: {ui: false, auth_token: x}\nproviders: {p: {type: ollama}}\nagents: {a: {}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, err := tenant.New(ctx, cfg, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(ctx, m, cfg))
	defer srv.Close()
	for _, p := range []string{"/", "/ui/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s with ui: false = %d, want 404", p, resp.StatusCode)
		}
	}
}
