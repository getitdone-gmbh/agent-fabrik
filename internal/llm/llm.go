// Package llm defines a provider-neutral chat interface with tool calling and
// small, dependency-free adapters for Anthropic, OpenAI-compatible APIs
// (OpenAI, Mistral, Groq, OpenRouter, Gemini, DeepSeek, vLLM, LM Studio, ...)
// and Ollama.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Role of a message in a conversation.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall is a model's request to invoke a tool.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message is one turn in a conversation.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`

	// Assistant turns.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// Tool result turns.
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`

	// Native holds the provider's own representation of an assistant turn
	// (e.g. Anthropic content blocks including thinking blocks) so it can be
	// replayed byte-for-byte. NativeProvider names the provider type that
	// produced it; other providers ignore it.
	Native         json.RawMessage `json:"native,omitempty"`
	NativeProvider string          `json:"native_provider,omitempty"`
}

// ToolSpec describes a tool offered to the model.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema (type: object)
}

// Request is a single chat completion request.
type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature *float64
}

// Usage reports token consumption.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
}

// Response is the model's reply.
type Response struct {
	Message    Message
	StopReason string // "end", "tool_calls", "max_tokens", "refusal", or provider-specific
	Usage      Usage
}

// Provider is a chat model backend.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req Request) (*Response, error)
}

// Config configures a provider instance.
type Config struct {
	Type         string            `yaml:"type"`
	BaseURL      string            `yaml:"base_url"`
	APIKey       string            `yaml:"api_key"`
	Model        string            `yaml:"model"` // default model
	Headers      map[string]string `yaml:"headers"`
	MaxTokens    int               `yaml:"max_tokens"`
	TimeoutSec   int               `yaml:"timeout_seconds"`
	MaxRetries   *int              `yaml:"max_retries"`
	Fallbacks    string            `yaml:"fallbacks"`    // anthropic: "default" (default) or "off"
	Effort       string            `yaml:"effort"`       // anthropic: low|medium|high|xhigh|max
	KeepAlive    string            `yaml:"keep_alive"`   // ollama
	ContextSize  int               `yaml:"context_size"` // ollama num_ctx
	APIVersion   string            `yaml:"api_version"`  // azure openai
	Organization string            `yaml:"organization"` // openai

	// Transport overrides the HTTP transport (e.g. an SSRF-guarded one for
	// tenant-defined providers). Not configurable from YAML.
	Transport http.RoundTripper `yaml:"-" json:"-"`
}

// openAIPresets maps provider type names to OpenAI-compatible base URLs.
var openAIPresets = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"mistral":    "https://api.mistral.ai/v1",
	"groq":       "https://api.groq.com/openai/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"gemini":     "https://generativelanguage.googleapis.com/v1beta/openai",
	"deepseek":   "https://api.deepseek.com/v1",
	"together":   "https://api.together.xyz/v1",
	"scaleway":   "https://api.scaleway.ai/v1",
	"ionos":      "https://openai.inference.de-txl.ionos.com/v1",
	"xai":        "https://api.x.ai/v1",
	"lmstudio":   "http://localhost:1234/v1",
	"vllm":       "http://localhost:8000/v1",
}

// Types lists every supported provider type.
func Types() []string {
	t := []string{"anthropic", "claude", "ollama", "openai-compatible"}
	for k := range openAIPresets {
		t = append(t, k)
	}
	return t
}

// New builds a provider from config.
func New(name string, c Config) (Provider, error) {
	typ := strings.ToLower(c.Type)
	switch typ {
	case "anthropic", "claude":
		return newAnthropic(name, c), nil
	case "ollama":
		return newOllama(name, c), nil
	case "openai-compatible", "openai_compatible":
		if c.BaseURL == "" {
			return nil, fmt.Errorf("provider %q: base_url is required for openai-compatible", name)
		}
		return newOpenAI(name, typ, c), nil
	}
	if base, ok := openAIPresets[typ]; ok {
		if c.BaseURL == "" {
			c.BaseURL = base
		}
		return newOpenAI(name, typ, c), nil
	}
	return nil, fmt.Errorf("provider %q: unknown type %q (supported: %s)", name, c.Type, strings.Join(Types(), ", "))
}

// argsOrEmpty returns a valid JSON object for tool arguments.
func argsOrEmpty(a json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(a))) == 0 {
		return json.RawMessage(`{}`)
	}
	return a
}

// schemaOrEmpty returns a valid JSON schema object.
func schemaOrEmpty(s json.RawMessage) json.RawMessage {
	if len(strings.TrimSpace(string(s))) == 0 || string(s) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return s
}
