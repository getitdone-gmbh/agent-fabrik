package llm

import (
	"context"
	"encoding/json"
	"strings"
)

const (
	anthropicDefaultBase  = "https://api.anthropic.com"
	anthropicDefaultModel = "claude-opus-5"
	anthropicVersion      = "2023-06-01"
	anthropicFallbackBeta = "server-side-fallback-2026-07-01"
)

// Anthropic speaks the Claude Messages API (POST /v1/messages).
type Anthropic struct {
	name string
	cfg  Config
	http httpClient
}

func newAnthropic(name string, c Config) *Anthropic {
	if c.BaseURL == "" {
		c.BaseURL = anthropicDefaultBase
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.Model == "" {
		c.Model = anthropicDefaultModel
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 16000
	}
	if c.Fallbacks == "" {
		c.Fallbacks = "default"
	}
	return &Anthropic{name: name, cfg: c, http: newHTTPClient(name, c)}
}

func (a *Anthropic) Name() string { return a.name }

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	System       string            `json:"system,omitempty"`
	Messages     []anthMessage     `json:"messages"`
	Tools        []anthTool        `json:"tools,omitempty"`
	Temperature  *float64          `json:"temperature,omitempty"`
	OutputConfig map[string]string `json:"output_config,omitempty"`
	Fallbacks    string            `json:"fallbacks,omitempty"`
}

type anthResponse struct {
	Content    json.RawMessage `json:"content"`
	StopReason string          `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (a *Anthropic) Chat(ctx context.Context, req Request) (*Response, error) {
	body := anthRequest{
		Model:       firstNonEmpty(req.Model, a.cfg.Model),
		MaxTokens:   firstPositive(req.MaxTokens, a.cfg.MaxTokens),
		System:      req.System,
		Messages:    toAnthropicMessages(req.Messages),
		Temperature: req.Temperature,
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthTool{Name: t.Name, Description: t.Description, InputSchema: schemaOrEmpty(t.Parameters)})
	}
	if a.cfg.Effort != "" {
		body.OutputConfig = map[string]string{"effort": a.cfg.Effort}
	}
	headers := map[string]string{"anthropic-version": anthropicVersion}
	if a.cfg.APIKey != "" {
		headers["x-api-key"] = a.cfg.APIKey
	}
	if a.cfg.Fallbacks != "off" && a.cfg.Fallbacks != "none" {
		// Route safety-classifier refusals to a fallback model server-side.
		body.Fallbacks = a.cfg.Fallbacks
		headers["anthropic-beta"] = anthropicFallbackBeta
	}
	for k, v := range a.cfg.Headers {
		headers[k] = v
	}

	var out anthResponse
	if err := a.http.postJSON(ctx, a.cfg.BaseURL+"/v1/messages", headers, body, &out); err != nil {
		return nil, err
	}
	var blocks []anthBlock
	if err := json.Unmarshal(out.Content, &blocks); err != nil {
		return nil, err
	}
	msg := Message{Role: RoleAssistant, Native: out.Content, NativeProvider: "anthropic"}
	var text []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: b.ID, Name: b.Name, Arguments: argsOrEmpty(b.Input)})
		}
	}
	msg.Content = strings.Join(text, "")

	stop := out.StopReason
	switch stop {
	case "end_turn", "stop_sequence":
		stop = "end"
	case "tool_use":
		stop = "tool_calls"
	}
	return &Response{
		Message:    msg,
		StopReason: stop,
		Usage:      Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens},
	}, nil
}

// toAnthropicMessages converts neutral messages, merging consecutive user-side
// turns (tool results + text) into single user messages as the API requires.
func toAnthropicMessages(msgs []Message) []anthMessage {
	var out []anthMessage
	var pending []anthBlock // user-side blocks awaiting flush

	flush := func() {
		if len(pending) == 0 {
			return
		}
		raw, _ := json.Marshal(pending)
		out = append(out, anthMessage{Role: "user", Content: raw})
		pending = nil
	}

	for _, m := range msgs {
		switch m.Role {
		case RoleUser:
			pending = append(pending, anthBlock{Type: "text", Text: m.Content})
		case RoleTool:
			content := m.Content
			if content == "" {
				content = "(empty result)"
			}
			pending = append(pending, anthBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content, IsError: m.IsError})
		case RoleAssistant:
			flush()
			if m.NativeProvider == "anthropic" && len(m.Native) > 0 {
				// Replay unchanged so thinking blocks stay valid.
				out = append(out, anthMessage{Role: "assistant", Content: m.Native})
				continue
			}
			var blocks []anthBlock
			if m.Content != "" {
				blocks = append(blocks, anthBlock{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthBlock{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: argsOrEmpty(tc.Arguments)})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, anthBlock{Type: "text", Text: "(no content)"})
			}
			raw, _ := json.Marshal(blocks)
			out = append(out, anthMessage{Role: "assistant", Content: raw})
		}
	}
	flush()
	return out
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositive(n ...int) int {
	for _, v := range n {
		if v > 0 {
			return v
		}
	}
	return 0
}
