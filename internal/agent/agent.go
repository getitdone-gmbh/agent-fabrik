// Package agent implements the tool-calling agent loop.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/llm"
	"github.com/getitdone-gmbh/agentd/internal/metrics"
	"github.com/getitdone-gmbh/agentd/internal/tools"
)

// Config defines an agent.
type Config struct {
	Name           string   `yaml:"-"`
	Description    string   `yaml:"description"`
	Provider       string   `yaml:"provider"`
	Model          string   `yaml:"model"`
	System         string   `yaml:"system"`
	Tools          []string `yaml:"tools"`
	MaxSteps       int      `yaml:"max_steps"`
	MaxTokens      int      `yaml:"max_tokens"`
	Temperature    *float64 `yaml:"temperature"`
	ToolTimeoutSec int      `yaml:"tool_timeout_seconds"`
	RunTimeoutSec  int      `yaml:"run_timeout_seconds"`
	MaxToolOutput  int      `yaml:"max_tool_output"`
	ParallelTools  int      `yaml:"parallel_tools"`
	HistoryLimit   int      `yaml:"history_limit"` // messages kept per session
	Expose         *bool    `yaml:"expose"`        // serve via HTTP/MCP/OpenAI API (default true)
}

// Exposed reports whether the agent is reachable through the public APIs.
func (c Config) Exposed() bool { return c.Expose == nil || *c.Expose }

// Event is a progress notification emitted while an agent runs.
type Event struct {
	Type    string          `json:"type"` // step, tool_call, tool_result, message, done, error
	Step    int             `json:"step,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Text    string          `json:"text,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

// RunInput is a request to run an agent.
type RunInput struct {
	Input     string
	SessionID string        // continue a conversation; empty starts a new one
	History   []llm.Message // explicit history (used instead of the store when set)
	OnEvent   func(Event)
}

// RunResult is the outcome of a run.
type RunResult struct {
	Output     string    `json:"output"`
	SessionID  string    `json:"session_id"`
	Steps      int       `json:"steps"`
	ToolCalls  int       `json:"tool_calls"`
	StopReason string    `json:"stop_reason"`
	Usage      llm.Usage `json:"usage"`
	DurationMS int64     `json:"duration_ms"`
}

// Agent runs a model with tools.
type Agent struct {
	cfg      Config
	provider llm.Provider
	tools    []tools.Tool
	byName   map[string]tools.Tool
	store    Store
	log      *slog.Logger

	locksMu sync.Mutex
	locks   map[string]*sessionLock // refcounted, removed when unused
}

type sessionLock struct {
	mu   sync.Mutex
	refs int
}

// New builds an agent.
func New(cfg Config, p llm.Provider, reg *tools.Registry, store Store, log *slog.Logger) (*Agent, error) {
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 12
	}
	if cfg.ToolTimeoutSec <= 0 {
		cfg.ToolTimeoutSec = 60
	}
	if cfg.RunTimeoutSec <= 0 {
		cfg.RunTimeoutSec = 600
	}
	if cfg.MaxToolOutput <= 0 {
		cfg.MaxToolOutput = 20000
	}
	if cfg.ParallelTools <= 0 {
		cfg.ParallelTools = 4
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 60
	}
	a := &Agent{cfg: cfg, provider: p, store: store, log: log.With("agent", cfg.Name), byName: map[string]tools.Tool{}}
	if len(cfg.Tools) > 0 {
		a.tools = reg.Select(cfg.Tools)
		if len(a.tools) == 0 {
			a.log.Warn("tool patterns matched no tools", "patterns", cfg.Tools)
		}
	}
	for _, t := range a.tools {
		a.byName[t.Name] = t
	}
	return a, nil
}

func (a *Agent) Config() Config         { return a.cfg }
func (a *Agent) Tools() []tools.Tool    { return a.tools }
func (a *Agent) Provider() llm.Provider { return a.provider }

// NewSessionID returns a random session identifier.
func NewSessionID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "s_" + hex.EncodeToString(b)
}

// lock serializes runs of the same session.
func (a *Agent) lock(id string) func() {
	a.locksMu.Lock()
	if a.locks == nil {
		a.locks = map[string]*sessionLock{}
	}
	l := a.locks[id]
	if l == nil {
		l = &sessionLock{}
		a.locks[id] = l
	}
	l.refs++
	a.locksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		a.locksMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(a.locks, id)
		}
		a.locksMu.Unlock()
	}
}

// Run executes the agent loop until the model answers without tool calls or
// the step limit is reached.
func (a *Agent) Run(ctx context.Context, in RunInput) (*RunResult, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.RunTimeoutSec)*time.Second)
	defer cancel()
	emit := in.OnEvent
	if emit == nil {
		emit = func(Event) {}
	}

	sid := in.SessionID
	persist := in.History == nil
	if sid == "" {
		sid = NewSessionID()
	}
	var history []llm.Message
	if persist {
		defer a.lock(sid)()
		h, err := a.store.Load(ctx, a.cfg.Name, sid)
		if err != nil {
			return nil, fmt.Errorf("load session: %w", err)
		}
		history = h
	} else {
		history = append(history, in.History...)
	}
	if strings.TrimSpace(in.Input) != "" {
		history = append(history, llm.Message{Role: llm.RoleUser, Content: in.Input})
	}
	if len(history) == 0 {
		return nil, errors.New("empty input")
	}

	specs := make([]llm.ToolSpec, len(a.tools))
	for i, t := range a.tools {
		specs[i] = llm.ToolSpec{Name: t.Name, Description: t.Description, Parameters: t.Schema}
	}

	res := &RunResult{SessionID: sid}
	metrics.RunsStarted.Add(1)
	for step := 1; ; step++ {
		res.Steps = step
		emit(Event{Type: "step", Step: step})
		resp, err := a.provider.Chat(ctx, llm.Request{
			Model:       a.cfg.Model,
			System:      a.cfg.System,
			Messages:    history,
			Tools:       specs,
			MaxTokens:   a.cfg.MaxTokens,
			Temperature: a.cfg.Temperature,
		})
		if err != nil {
			metrics.RunsFailed.Add(1)
			emit(Event{Type: "error", Text: err.Error()})
			res.DurationMS = time.Since(start).Milliseconds()
			return res, err // partial result carries usage so far
		}
		metrics.LLMCalls.Add(1)
		metrics.InputTokens.Add(int64(resp.Usage.InputTokens))
		metrics.OutputTokens.Add(int64(resp.Usage.OutputTokens))
		res.Usage.Add(resp.Usage)
		history = append(history, resp.Message)
		if resp.Message.Content != "" {
			emit(Event{Type: "message", Step: step, Text: resp.Message.Content})
		}
		res.Output = resp.Message.Content
		res.StopReason = resp.StopReason

		if len(resp.Message.ToolCalls) == 0 {
			break
		}
		if step >= a.cfg.MaxSteps {
			// Answer the pending calls so the history stays well-formed.
			for _, tc := range resp.Message.ToolCalls {
				history = append(history, llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, ToolName: tc.Name, Content: "step limit reached; tool not executed", IsError: true})
			}
			res.StopReason = "max_steps"
			if res.Output == "" {
				res.Output = "(stopped: step limit reached before a final answer)"
			}
			break
		}
		results := a.execTools(ctx, step, resp.Message.ToolCalls, emit)
		res.ToolCalls += len(results)
		history = append(history, results...)
	}

	if persist {
		if err := a.store.Save(ctx, a.cfg.Name, sid, trimHistory(history, a.cfg.HistoryLimit)); err != nil {
			a.log.Warn("save session failed", "session", sid, "err", err)
		}
	}
	res.DurationMS = time.Since(start).Milliseconds()
	metrics.RunsSucceeded.Add(1)
	emit(Event{Type: "done", Text: res.Output})
	a.log.Info("run finished", "session", sid, "steps", res.Steps, "tool_calls", res.ToolCalls,
		"input_tokens", res.Usage.InputTokens, "output_tokens", res.Usage.OutputTokens, "ms", res.DurationMS, "stop", res.StopReason)
	return res, nil
}

func (a *Agent) execTools(ctx context.Context, step int, calls []llm.ToolCall, emit func(Event)) []llm.Message {
	out := make([]llm.Message, len(calls))
	sem := make(chan struct{}, a.cfg.ParallelTools)
	var wg sync.WaitGroup
	var emu sync.Mutex
	safeEmit := func(e Event) { emu.Lock(); emit(e); emu.Unlock() }
	for i, tc := range calls {
		safeEmit(Event{Type: "tool_call", Step: step, Tool: tc.Name, CallID: tc.ID, Args: tc.Arguments})
		wg.Add(1)
		go func(i int, tc llm.ToolCall) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			text, err := a.callTool(ctx, tc)
			msg := llm.Message{Role: llm.RoleTool, ToolCallID: tc.ID, ToolName: tc.Name, Content: text}
			if err != nil {
				msg.Content, msg.IsError = err.Error(), true
				metrics.ToolErrors.Add(1)
			}
			if len(msg.Content) > a.cfg.MaxToolOutput {
				msg.Content = msg.Content[:a.cfg.MaxToolOutput] + "\n…(truncated)"
			}
			metrics.ToolCalls.Add(1)
			out[i] = msg
			safeEmit(Event{Type: "tool_result", Step: step, Tool: tc.Name, CallID: tc.ID, Text: msg.Content, IsError: msg.IsError})
		}(i, tc)
	}
	wg.Wait()
	return out
}

func (a *Agent) callTool(ctx context.Context, tc llm.ToolCall) (text string, err error) {
	t, ok := a.byName[tc.Name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", tc.Name)
	}
	if !json.Valid(tc.Arguments) {
		return "", fmt.Errorf("tool arguments are not valid JSON")
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tool panicked: %v", r)
			a.log.Error("tool panic", "tool", tc.Name, "panic", r)
		}
	}()
	tctx, cancel := context.WithTimeout(ctx, time.Duration(a.cfg.ToolTimeoutSec)*time.Second)
	defer cancel()
	start := time.Now()
	text, err = t.Handler(tctx, tc.Arguments)
	a.log.Debug("tool call", "tool", tc.Name, "ms", time.Since(start).Milliseconds(), "err", err)
	return text, err
}

// trimHistory keeps at most limit messages, cutting only at user turns so
// tool calls and their results stay paired.
func trimHistory(h []llm.Message, limit int) []llm.Message {
	if len(h) <= limit {
		return h
	}
	for i := len(h) - limit; i < len(h); i++ {
		if h[i].Role == llm.RoleUser {
			return append([]llm.Message(nil), h[i:]...)
		}
	}
	// No user turn in the window: keep the last user turn onwards.
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == llm.RoleUser {
			return append([]llm.Message(nil), h[i:]...)
		}
	}
	return h
}
