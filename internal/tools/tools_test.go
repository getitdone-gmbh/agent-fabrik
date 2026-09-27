package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQualifiedName(t *testing.T) {
	if n := QualifiedName("my.srv", "do/thing"); n != "my_srv__do_thing" {
		t.Error(n)
	}
	if n := QualifiedName("c", strings.Repeat("x", 100)); len(n) != 64 {
		t.Error(len(n))
	}
}

func TestRegistrySelect(t *testing.T) {
	r := NewRegistry()
	r.Add(context.Background(), NewBuiltin("system"))
	if got := r.Select([]string{"system.*"}); len(got) != 1 || got[0].Name != "system__now" {
		t.Errorf("%v", got)
	}
	if got := r.Select([]string{"other.*"}); len(got) != 0 {
		t.Errorf("%v", got)
	}
	if got := r.Select([]string{"system__now"}); len(got) != 1 {
		t.Errorf("select by qualified name: %v", got)
	}
}

func TestHTTPEndpointsAndGeneric(t *testing.T) {
	var last *http.Request
	var lastBody string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer api.Close()

	c, err := NewHTTPConnector("api", HTTPConfig{
		BaseURL:    api.URL + "/v2",
		Headers:    map[string]string{"Authorization": "Bearer secret"},
		AllowHosts: []string{"example.org"},
		Endpoints: []Endpoint{
			{Name: "get_user", Method: "GET", Path: "/users/{id}", Params: map[string]Param{"id": {}, "expand": {Type: "boolean"}}},
			{Name: "create_ticket", Method: "POST", Path: "/tickets", Params: map[string]Param{"title": {Required: true}, "prio": {Type: "integer"}, "dry": {In: "query", Type: "boolean"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := c.Tools(context.Background())
	byName := map[string]Tool{}
	for _, tl := range ts {
		byName[tl.Local] = tl
	}
	if len(ts) != 3 {
		t.Fatalf("tools: %d", len(ts))
	}
	var schema map[string]any
	json.Unmarshal(byName["get_user"].Schema, &schema)
	if req := schema["required"].([]any); len(req) != 1 || req[0] != "id" {
		t.Errorf("schema: %v", schema)
	}

	out, err := byName["get_user"].Handler(context.Background(), json.RawMessage(`{"id":"a b","expand":true}`))
	if err != nil || !strings.Contains(out, "HTTP 200") {
		t.Fatal(out, err)
	}
	if last.URL.EscapedPath() != "/v2/users/a%20b" || last.URL.Query().Get("expand") != "true" || last.Header.Get("Authorization") != "Bearer secret" {
		t.Errorf("request: %s %v", last.URL, last.Header)
	}

	_, err = byName["create_ticket"].Handler(context.Background(), json.RawMessage(`{"title":"x","prio":2,"dry":true}`))
	if err != nil || last.Method != "POST" || last.URL.Query().Get("dry") != "true" || lastBody != `{"prio":2,"title":"x"}` {
		t.Errorf("post: %v %s %s", err, last.URL, lastBody)
	}

	// Generic request: relative path against base, credentials attached.
	_, err = byName["request"].Handler(context.Background(), json.RawMessage(`{"url":"/health?x=1"}`))
	if err != nil || last.URL.Path != "/v2/health" || last.URL.RawQuery != "x=1" || last.Header.Get("Authorization") == "" {
		t.Errorf("generic: %v %s", err, last.URL)
	}
	// Disallowed host.
	if _, err := byName["request"].Handler(context.Background(), json.RawMessage(`{"url":"https://evil.com/"}`)); err == nil {
		t.Error("expected allowlist rejection")
	}
	if _, err := byName["request"].Handler(context.Background(), json.RawMessage(`{"url":"file:///etc/passwd"}`)); err == nil {
		t.Error("expected scheme rejection")
	}
}

func TestHTTPGenericBlocksPrivateAddresses(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer api.Close()
	c, _ := NewHTTPConnector("web", HTTPConfig{AllowHosts: []string{"*"}})
	ts, _ := c.Tools(context.Background())
	_, err := ts[0].Handler(context.Background(), json.RawMessage(`{"url":"`+api.URL+`"}`))
	if err == nil || !strings.Contains(err.Error(), "internal address") {
		t.Fatalf("expected SSRF block, got %v", err)
	}
	c2, _ := NewHTTPConnector("web", HTTPConfig{AllowHosts: []string{"*"}, AllowPrivate: true})
	ts, _ = c2.Tools(context.Background())
	if _, err := ts[0].Handler(context.Background(), json.RawMessage(`{"url":"`+api.URL+`"}`)); err != nil {
		t.Fatalf("allow_private: %v", err)
	}
}

func TestHTTPTruncation(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("a", 5000))
	}))
	defer api.Close()
	c, _ := NewHTTPConnector("x", HTTPConfig{BaseURL: api.URL, MaxResponseBytes: 100})
	ts, _ := c.Tools(context.Background())
	out, _ := ts[0].Handler(context.Background(), json.RawMessage(`{"url":"/"}`))
	if !strings.HasSuffix(out, "(truncated)") || len(out) > 200 {
		t.Errorf("len %d", len(out))
	}
}

func TestEmailRecipientAllowlist(t *testing.T) {
	e, _ := NewEmailConnector("m", EmailConfig{SMTP: nil, IMAP: nil})
	if e != nil {
		t.Fatal("expected config error")
	}
	e2 := &EmailConnector{cfg: EmailConfig{AllowRecipients: []string{"*@example.com"}}}
	if !e2.recipientAllowed("Bob <bob@Example.com>") || e2.recipientAllowed("x@evil.com") {
		t.Error("allowlist")
	}
}
