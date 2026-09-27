package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func conversation() Request {
	return Request{
		System: "be brief",
		Messages: []Message{
			{Role: RoleUser, Content: "weather in Berlin and Paris?"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "toolu_1", Name: "weather", Arguments: json.RawMessage(`{"city":"Berlin"}`)},
				{ID: "toolu_2", Name: "weather", Arguments: json.RawMessage(`{"city":"Paris"}`)},
			}},
			{Role: RoleTool, ToolCallID: "toolu_1", ToolName: "weather", Content: "12C"},
			{Role: RoleTool, ToolCallID: "toolu_2", ToolName: "weather", Content: "boom", IsError: true},
		},
		Tools: []ToolSpec{{Name: "weather", Description: "get weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
	}
}

func TestAnthropicRequestAndResponse(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path %s", r.URL.Path)
		}
		hdr = r.Header
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_3","name":"weather","input":{"city":"Rome"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
	}))
	defer srv.Close()

	p, err := New("claude", Config{Type: "anthropic", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := p.Chat(context.Background(), conversation())
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Get("x-api-key") != "k" || hdr.Get("anthropic-version") != anthropicVersion || hdr.Get("anthropic-beta") != anthropicFallbackBeta {
		t.Errorf("headers: %v", hdr)
	}
	if got["model"] != anthropicDefaultModel || got["system"] != "be brief" || got["fallbacks"] != "default" {
		t.Errorf("body: %v", got)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (user, assistant, merged tool results), got %d: %v", len(msgs), msgs)
	}
	results := msgs[2].(map[string]any)["content"].([]any)
	if len(results) != 2 || results[1].(map[string]any)["is_error"] != true {
		t.Errorf("tool results not merged: %v", results)
	}
	if resp.StopReason != "tool_calls" || len(resp.Message.ToolCalls) != 1 || resp.Message.Content != "Checking." {
		t.Errorf("resp: %+v", resp)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("usage %+v", resp.Usage)
	}
	// Native blocks (incl. thinking) must be replayed verbatim.
	req2 := conversation()
	req2.Messages = append(req2.Messages[:1], resp.Message)
	if _, err := p.Chat(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	asst := got["messages"].([]any)[1].(map[string]any)["content"].([]any)
	if asst[0].(map[string]any)["signature"] != "sig" {
		t.Errorf("thinking block not replayed: %v", asst)
	}
}

func TestOpenAICompatibleMistral(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer mk" {
			t.Errorf("bad request %s %v", r.URL.Path, r.Header)
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"abcDEF123","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Rome\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer srv.Close()
	p, _ := New("m", Config{Type: "mistral", BaseURL: srv.URL + "/v1", APIKey: "mk", Model: "mistral-large-latest"})
	resp, err := p.Chat(context.Background(), conversation())
	if err != nil {
		t.Fatal(err)
	}
	msgs := got["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || len(msgs) != 5 {
		t.Fatalf("messages: %v", msgs)
	}
	// Mistral needs 9-char alphanumeric tool call ids, consistently mapped.
	call := msgs[2].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["id"].(string)
	res := msgs[3].(map[string]any)["tool_call_id"].(string)
	if len(call) != 9 || call != res {
		t.Errorf("ids not adapted: %q %q", call, res)
	}
	if got["max_tokens"] == nil {
		t.Error("max_tokens missing")
	}
	if resp.StopReason != "tool_calls" || string(resp.Message.ToolCalls[0].Arguments) != `{"city":"Rome"}` {
		t.Errorf("resp: %+v", resp)
	}
}

func TestOllamaNative(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"weather","arguments":{"city":"Oslo"}}}]},"done":true,"done_reason":"stop","prompt_eval_count":4,"eval_count":2}`)
	}))
	defer srv.Close()
	p, _ := New("o", Config{Type: "ollama", BaseURL: srv.URL + "/v1", Model: "qwen3", ContextSize: 8192})
	resp, err := p.Chat(context.Background(), conversation())
	if err != nil {
		t.Fatal(err)
	}
	if got["stream"] != false || got["options"].(map[string]any)["num_ctx"].(float64) != 8192 {
		t.Errorf("body: %v", got)
	}
	tool := got["messages"].([]any)[3].(map[string]any)
	if tool["role"] != "tool" || tool["tool_name"] != "weather" {
		t.Errorf("tool msg: %v", tool)
	}
	if resp.StopReason != "tool_calls" || resp.Message.ToolCalls[0].ID == "" {
		t.Errorf("resp: %+v", resp)
	}
}

func TestRetryOn429(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()
	p, _ := New("x", Config{Type: "openai-compatible", BaseURL: srv.URL})
	resp, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil || resp.Message.Content != "hi" || n.Load() != 2 {
		t.Fatalf("resp=%v err=%v calls=%d", resp, err, n.Load())
	}
}

func TestNoRetryOn400(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(400)
		io.WriteString(w, `{"error":"bad"}`)
	}))
	defer srv.Close()
	p, _ := New("x", Config{Type: "anthropic", BaseURL: srv.URL})
	_, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") || n.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, n.Load())
	}
}

func TestUnknownProvider(t *testing.T) {
	if _, err := New("x", Config{Type: "nope"}); err == nil {
		t.Fatal("expected error")
	}
}
