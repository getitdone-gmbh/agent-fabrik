package server_test

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/app"
	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/mcp"
	"github.com/getitdone-gmbh/agent-fabrik/internal/server"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tenant"
	"github.com/getitdone-gmbh/agent-fabrik/internal/testutil"
)

// fakeLLM is an OpenAI-compatible model: on a user turn it calls the first
// tool whose name contains "echo" (if offered); on a tool turn it answers
// with the tool output.
func fakeLLM(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("llm decode: %v", err)
		}
		last := req.Messages[len(req.Messages)-1]
		msg := map[string]any{"role": "assistant"}
		finish := "stop"
		echo := ""
		for _, tl := range req.Tools {
			if strings.Contains(tl.Function.Name, "echo") {
				echo = tl.Function.Name
			}
		}
		reader := ""
		for _, tl := range req.Tools {
			if tl.Function.Name == "files__read" {
				reader = tl.Function.Name
			}
		}
		switch {
		case last.Role == "user" && reader != "" && strings.HasPrefix(last.Content, "READ "):
			args, _ := json.Marshal(map[string]string{"path": strings.TrimPrefix(last.Content, "READ ")})
			msg["content"] = nil
			msg["tool_calls"] = []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": reader, "arguments": string(args)}}}
			finish = "tool_calls"
		case last.Role == "user" && echo != "":
			first, _, _ := strings.Cut(last.Content, "\n")
			args, _ := json.Marshal(map[string]string{"text": first})
			msg["content"] = nil
			msg["tool_calls"] = []map[string]any{{"id": "call_1", "type": "function", "function": map[string]any{"name": echo, "arguments": string(args)}}}
			finish = "tool_calls"
		case last.Role == "tool":
			msg["content"] = "ANSWER: " + last.Content
		default:
			msg["content"] = "ANSWER: " + last.Content
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
			"usage":   map[string]int{"prompt_tokens": 10, "completion_tokens": 3},
		})
	}))
}

func toolServer() *httptest.Server {
	s := &mcp.Server{Name: "tools", Version: "1", Tools: func() []mcp.ServerTool {
		return []mcp.ServerTool{{
			Name: "echo", Description: "echo text",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
			Handler: func(_ context.Context, a json.RawMessage) (string, error) {
				var v struct{ Text string }
				json.Unmarshal(a, &v)
				return "echo: " + v.Text, nil
			},
		}}
	}}
	return httptest.NewServer(s)
}

type env struct {
	srv  *httptest.Server
	imap *testutil.FakeIMAP
	smtp *testutil.FakeSMTP
}

func setup(t *testing.T) *env {
	llmSrv := fakeLLM(t)
	t.Cleanup(llmSrv.Close)
	mcpSrv := toolServer()
	t.Cleanup(mcpSrv.Close)
	im := testutil.NewFakeIMAP(t, "bot@example.com", "pw")
	sm := testutil.NewFakeSMTP(t)
	ih, ip, _ := strings.Cut(im.Addr, ":")
	sh, sp, _ := strings.Cut(sm.Addr, ":")

	doc := fmt.Sprintf(`
server:
  auth_token: secret-token
providers:
  fake:
    type: openai-compatible
    base_url: %s
connectors:
  tools:
    type: mcp
    url: %s
  mail:
    type: email
    smtp: {host: %s, port: %s, tls: none, from: "Bot <bot@example.com>"}
    imap: {host: %s, port: %s, tls: none, username: bot@example.com, password: pw}
agents:
  bot:
    description: Echo bot
    tools: ["tools.*"]
    triggers:
      - type: webhook
        name: gh
        secret: hooksecret
  mailbot:
    tools: ["tools.*"]
    expose: false
    triggers:
      - type: imap
        connector: mail
        every: 200ms
`, llmSrv.URL, mcpSrv.URL, sh, sp, ih, ip)
	cfg, err := config.Parse([]byte(doc))
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
	var wg sync.WaitGroup
	m.Start(ctx, &wg)
	srv := httptest.NewServer(server.New(ctx, m, cfg))
	t.Cleanup(func() {
		srv.Close()
		cancel()
		wg.Wait()
		a.Close()
	})
	return &env{srv: srv, imap: im, smtp: sm}
}

func (e *env) do(t *testing.T, method, path, body string, hdr map[string]string) (*http.Response, string) {
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

var auth = map[string]string{"Authorization": "Bearer secret-token"}

func TestEndToEnd(t *testing.T) {
	e := setup(t)

	t.Run("auth required", func(t *testing.T) {
		resp, _ := e.do(t, "POST", "/v1/agents/bot/run", `{"input":"hi"}`, nil)
		if resp.StatusCode != 401 {
			t.Errorf("status %d", resp.StatusCode)
		}
	})

	t.Run("health and metrics", func(t *testing.T) {
		resp, _ := e.do(t, "GET", "/healthz", "", nil)
		if resp.StatusCode != 200 {
			t.Error(resp.StatusCode)
		}
	})

	t.Run("list agents hides unexposed", func(t *testing.T) {
		_, body := e.do(t, "GET", "/v1/agents", "", auth)
		if !strings.Contains(body, `"bot"`) || strings.Contains(body, "mailbot") || !strings.Contains(body, "tools__echo") {
			t.Errorf("body: %s", body)
		}
		resp, _ := e.do(t, "POST", "/v1/agents/mailbot/run", `{"input":"x"}`, auth)
		if resp.StatusCode != 404 {
			t.Errorf("unexposed agent reachable: %d", resp.StatusCode)
		}
	})

	t.Run("run via MCP tool", func(t *testing.T) {
		resp, body := e.do(t, "POST", "/v1/agents/bot/run", `{"input":"hello world"}`, auth)
		var res struct {
			Output    string `json:"output"`
			SessionID string `json:"session_id"`
			ToolCalls int    `json:"tool_calls"`
		}
		json.Unmarshal([]byte(body), &res)
		if resp.StatusCode != 200 || res.Output != "ANSWER: echo: hello world" || res.ToolCalls != 1 || res.SessionID == "" {
			t.Errorf("%d %s", resp.StatusCode, body)
		}
	})

	t.Run("sse stream", func(t *testing.T) {
		req, _ := http.NewRequest("POST", e.srv.URL+"/v1/agents/bot/run", strings.NewReader(`{"input":"stream me","stream":true}`))
		req.Header.Set("Authorization", "Bearer secret-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var events []string
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if ev, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
				events = append(events, ev)
			}
		}
		got := strings.Join(events, ",")
		if got != "step,tool_call,tool_result,step,message,result" {
			t.Errorf("events: %s", got)
		}
	})

	t.Run("openai compatible", func(t *testing.T) {
		_, body := e.do(t, "POST", "/v1/chat/completions", `{"model":"bot","messages":[{"role":"system","content":"ctx"},{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":[{"type":"text","text":"second"}]}]}`, auth)
		var res struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
			Usage struct {
				TotalTokens int `json:"total_tokens"`
			} `json:"usage"`
		}
		json.Unmarshal([]byte(body), &res)
		if len(res.Choices) != 1 || !strings.HasPrefix(res.Choices[0].Message.Content, "ANSWER: echo: Context from the caller:") || res.Usage.TotalTokens == 0 {
			t.Errorf("body: %s", body)
		}
		_, models := e.do(t, "GET", "/v1/models", "", auth)
		if !strings.Contains(models, `"id":"bot"`) {
			t.Errorf("models: %s", models)
		}
		_, stream := e.do(t, "POST", "/v1/chat/completions", `{"model":"bot","stream":true,"messages":[{"role":"user","content":"x"}]}`, auth)
		if !strings.Contains(stream, "chat.completion.chunk") || !strings.HasSuffix(strings.TrimSpace(stream), "data: [DONE]") {
			t.Errorf("stream: %s", stream)
		}
	})

	t.Run("mcp server exposes agents", func(t *testing.T) {
		c := mcp.NewHTTPClient(e.srv.URL+"/mcp", auth, 0, nil)
		ctx := context.Background()
		if err := c.Initialize(ctx); err != nil {
			t.Fatal(err)
		}
		ts, err := c.ListTools(ctx)
		if err != nil || len(ts) != 1 || ts[0].Name != "bot" {
			t.Fatalf("tools: %+v %v", ts, err)
		}
		res, err := c.CallTool(ctx, "bot", json.RawMessage(`{"input":"via mcp"}`))
		if err != nil || !strings.HasPrefix(res.Text(), "ANSWER: echo: via mcp") || !strings.Contains(res.Text(), "session_id") {
			t.Errorf("%+v %v", res, err)
		}
		bad := mcp.NewHTTPClient(e.srv.URL+"/mcp", nil, 0, nil)
		if err := bad.Initialize(ctx); err == nil {
			t.Error("mcp without token should fail")
		}
	})

	t.Run("webhook hmac", func(t *testing.T) {
		payload := `{"action":"opened"}`
		mac := hmac.New(sha256.New, []byte("hooksecret"))
		mac.Write([]byte(payload))
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		resp, body := e.do(t, "POST", "/v1/hooks/bot/gh", payload, map[string]string{"X-Hub-Signature-256": sig, "X-GitHub-Event": "issues"})
		if resp.StatusCode != 200 || !strings.Contains(body, "ANSWER: echo: A webhook was received") {
			t.Errorf("%d %s", resp.StatusCode, body)
		}
		resp, _ = e.do(t, "POST", "/v1/hooks/bot/gh", payload, map[string]string{"X-Hub-Signature-256": "sha256=00"})
		if resp.StatusCode != 401 {
			t.Errorf("bad signature accepted: %d", resp.StatusCode)
		}
		resp, _ = e.do(t, "POST", "/v1/hooks/bot/gh?secret=hooksecret", payload, nil)
		if resp.StatusCode != 200 {
			t.Errorf("shared secret: %d", resp.StatusCode)
		}
	})

	t.Run("imap trigger replies by smtp", func(t *testing.T) {
		uid := e.imap.Add("From: Alice <alice@example.org>\nTo: bot@example.com\nSubject: Need help\nMessage-ID: <q1@example.org>\n\nPlease help me\n")
		auto := e.imap.Add("From: MAILER-DAEMON@example.org\nSubject: Undeliverable\nMessage-ID: <b1@example.org>\n\nbounce\n")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && (len(e.smtp.Sent()) == 0 || !e.imap.Seen(uid) || !e.imap.Seen(auto)) {
			time.Sleep(50 * time.Millisecond)
		}
		sent := e.smtp.Sent()
		if len(sent) != 1 {
			t.Fatalf("sent %d mails", len(sent))
		}
		d := sent[0].Data
		for _, want := range []string{"To: \"Alice\" <alice@example.org>", "Subject: Re: Need help", "In-Reply-To: <q1@example.org>", "Auto-Submitted: auto-replied", "ANSWER: echo: New email received."} {
			if !strings.Contains(d, want) {
				t.Errorf("reply missing %q:\n%s", want, d)
			}
		}
		if !e.imap.Seen(uid) || !e.imap.Seen(auto) {
			t.Error("messages not marked seen")
		}
	})
}
