package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/llm"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tools"
)

// scripted returns canned responses and records requests.
type scripted struct {
	mu    sync.Mutex
	steps []llm.Response
	reqs  []llm.Request
}

func (s *scripted) Name() string { return "scripted" }
func (s *scripted) Chat(_ context.Context, r llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, r)
	if len(s.steps) == 0 {
		return nil, errors.New("script exhausted")
	}
	resp := s.steps[0]
	s.steps = s.steps[1:]
	return &resp, nil
}

type testConn struct{ calls chan string }

func (c *testConn) Name() string { return "t" }
func (c *testConn) Close() error { return nil }
func (c *testConn) Tools(context.Context) ([]tools.Tool, error) {
	return []tools.Tool{
		{Local: "add", Description: "adds", Handler: func(_ context.Context, a json.RawMessage) (string, error) {
			var v struct{ A, B int }
			json.Unmarshal(a, &v)
			c.calls <- "add"
			return string(rune('0' + v.A + v.B)), nil
		}},
		{Local: "slow", Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}},
		{Local: "boom", Handler: func(context.Context, json.RawMessage) (string, error) { panic("kaboom") }},
	}, nil
}

func newTestAgent(t *testing.T, p llm.Provider, cfg Config) (*Agent, *testConn) {
	reg := tools.NewRegistry()
	conn := &testConn{calls: make(chan string, 10)}
	reg.Add(context.Background(), conn)
	cfg.Name = "test"
	if cfg.Tools == nil {
		cfg.Tools = []string{"t.*"}
	}
	a, err := New(cfg, p, reg, NewMemoryStore(time.Hour, 10), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a, conn
}

func call(id, name, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestAgentLoop(t *testing.T) {
	p := &scripted{steps: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("1", "t__add", `{"a":2,"b":3}`), call("2", "t__boom", `{}`), call("3", "t__missing", `{}`)}}, StopReason: "tool_calls", Usage: llm.Usage{InputTokens: 5, OutputTokens: 1}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "2+3=5"}, StopReason: "end", Usage: llm.Usage{InputTokens: 9, OutputTokens: 2}},
	}}
	a, _ := newTestAgent(t, p, Config{System: "sys"})
	var events []string
	res, err := a.Run(context.Background(), RunInput{Input: "add 2 and 3", OnEvent: func(e Event) { events = append(events, e.Type) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "2+3=5" || res.Steps != 2 || res.ToolCalls != 3 || res.Usage.InputTokens != 14 {
		t.Errorf("result: %+v", res)
	}
	second := p.reqs[1].Messages
	if len(second) != 5 {
		t.Fatalf("history: %+v", second)
	}
	if second[2].Content != "5" || second[2].IsError {
		t.Errorf("add result: %+v", second[2])
	}
	if !second[3].IsError || !second[4].IsError {
		t.Errorf("panic/unknown should be errors: %+v %+v", second[3], second[4])
	}
	if p.reqs[0].System != "sys" || len(p.reqs[0].Tools) != 3 {
		t.Errorf("request: %+v", p.reqs[0])
	}
	if events[0] != "step" || events[len(events)-1] != "done" {
		t.Errorf("events: %v", events)
	}

	// Session memory: the next run sees the prior turns.
	p.steps = []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, Content: "yes"}, StopReason: "end"}}
	if _, err := a.Run(context.Background(), RunInput{Input: "sure?", SessionID: res.SessionID}); err != nil {
		t.Fatal(err)
	}
	if n := len(p.reqs[2].Messages); n != 7 {
		t.Errorf("continued session should have 7 messages, got %d", n)
	}
}

func TestAgentMaxStepsAndToolTimeout(t *testing.T) {
	loop := llm.Response{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("x", "t__slow", `{}`)}}, StopReason: "tool_calls"}
	p := &scripted{steps: []llm.Response{loop, loop, loop}}
	a, _ := newTestAgent(t, p, Config{MaxSteps: 2, ToolTimeoutSec: 1})
	start := time.Now()
	res, err := a.Run(context.Background(), RunInput{Input: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if res.StopReason != "max_steps" || res.Steps != 2 {
		t.Errorf("result: %+v", res)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("tool timeout not enforced")
	}
	// History stays well-formed: the dangling call got a result.
	h, _ := a.store.Load(context.Background(), "test", res.SessionID)
	if last := h[len(h)-1]; last.Role != llm.RoleTool {
		t.Errorf("last message should be a tool result: %+v", last)
	}
}

func TestAgentProviderErrorNotPersisted(t *testing.T) {
	p := &scripted{}
	a, _ := newTestAgent(t, p, Config{})
	if _, err := a.Run(context.Background(), RunInput{Input: "hi", SessionID: "s1"}); err == nil {
		t.Fatal("expected error")
	}
	h, _ := a.store.Load(context.Background(), "test", "s1")
	if len(h) != 0 {
		t.Errorf("failed run should not persist: %+v", h)
	}
}

func TestTrimHistory(t *testing.T) {
	var h []llm.Message
	for i := 0; i < 5; i++ {
		h = append(h,
			llm.Message{Role: llm.RoleUser, Content: "q"},
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call("a", "t", "{}")}},
			llm.Message{Role: llm.RoleTool, ToolCallID: "a"},
			llm.Message{Role: llm.RoleAssistant, Content: "a"},
		)
	}
	got := trimHistory(h, 6)
	if len(got) != 4 || got[0].Role != llm.RoleUser {
		t.Errorf("trim: %d %+v", len(got), got[0])
	}
}

func TestFileStore(t *testing.T) {
	fs, err := NewFileStore(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	msgs := []llm.Message{{Role: llm.RoleUser, Content: "hi"}, {Role: llm.RoleAssistant, Content: "yo", Native: json.RawMessage(`[{"type":"text","text":"yo"}]`), NativeProvider: "anthropic"}}
	if err := fs.Save(ctx, "a", "s", msgs); err != nil {
		t.Fatal(err)
	}
	got, err := fs.Load(ctx, "a", "s")
	if err != nil || len(got) != 2 || string(got[1].Native) != `[{"type":"text","text":"yo"}]` {
		t.Fatalf("%+v %v", got, err)
	}
	fs.Delete(ctx, "a", "s")
	if got, _ := fs.Load(ctx, "a", "s"); got != nil {
		t.Error("not deleted")
	}
}

func TestMemoryStoreEviction(t *testing.T) {
	m := NewMemoryStore(time.Hour, 2)
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		m.Save(ctx, "x", s, []llm.Message{{Role: llm.RoleUser, Content: s}})
		time.Sleep(time.Millisecond)
	}
	if got, _ := m.Load(ctx, "x", "a"); got != nil {
		t.Error("oldest should be evicted")
	}
	if got, _ := m.Load(ctx, "x", "c"); got == nil {
		t.Error("newest missing")
	}
}

func TestSessionLocksAreReleased(t *testing.T) {
	var steps []llm.Response
	for i := 0; i < 20; i++ {
		steps = append(steps, llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}, StopReason: "end"})
	}
	a, _ := newTestAgent(t, &scripted{steps: steps}, Config{})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a.Run(context.Background(), RunInput{Input: "x", SessionID: []string{"a", "b", ""}[i%3]})
		}(i)
	}
	wg.Wait()
	a.locksMu.Lock()
	defer a.locksMu.Unlock()
	if len(a.locks) != 0 {
		t.Errorf("leaked %d session locks", len(a.locks))
	}
}

func TestImagesReachModelButNotStore(t *testing.T) {
	p := &scripted{steps: []llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "Ein Kratzer am Lack."}},
		{Message: llm.Message{Role: llm.RoleAssistant, Content: "Etwa 200 Euro."}},
	}}
	a, _ := newTestAgent(t, p, Config{})
	img := llm.Image{MediaType: "image/png", Data: "AAAA"}
	res, err := a.Run(context.Background(), RunInput{Input: "Was ist das?", Images: []llm.Image{img}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.reqs[0].Messages[0].Images; len(got) != 1 || got[0] != img {
		t.Fatalf("model did not get the image: %+v", p.reqs[0].Messages[0])
	}
	stored, _ := a.store.Load(context.Background(), "test", res.SessionID)
	if len(stored) == 0 || stored[0].Images != nil {
		t.Fatalf("image was stored: %+v", stored)
	}
	// The follow-up turn sees the note, not the picture again.
	if _, err := a.Run(context.Background(), RunInput{Input: "Was kostet das?", SessionID: res.SessionID}); err != nil {
		t.Fatal(err)
	}
	if m := p.reqs[1].Messages[0]; m.Images != nil || m.Content == "Was ist das?" {
		t.Errorf("second turn: %+v", m)
	}
}

func TestImageOnlyInput(t *testing.T) {
	p := &scripted{steps: []llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}}}
	a, _ := newTestAgent(t, p, Config{})
	if _, err := a.Run(context.Background(), RunInput{Images: []llm.Image{{MediaType: "image/png", Data: "AAAA"}}}); err != nil {
		t.Fatalf("image without text must run: %v", err)
	}
}
