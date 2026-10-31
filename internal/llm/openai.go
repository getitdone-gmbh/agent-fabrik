package llm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

// OpenAI speaks the Chat Completions API shared by OpenAI, Mistral, Groq,
// OpenRouter, Gemini (OpenAI endpoint), DeepSeek, vLLM, LM Studio, Azure, ...
type OpenAI struct {
	name   string
	flavor string
	cfg    Config
	http   httpClient
}

func newOpenAI(name, flavor string, c Config) *OpenAI {
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	if c.MaxTokens == 0 {
		c.MaxTokens = 8192
	}
	if c.Model == "" {
		switch flavor {
		case "mistral":
			c.Model = "mistral-medium-latest"
		case "openai":
			c.Model = "gpt-5"
		}
	}
	return &OpenAI{name: name, flavor: flavor, cfg: c, http: newHTTPClient(name, c)}
}

func (o *OpenAI) Name() string { return o.name }

type oaFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   *string         `json:"arguments,omitempty"`
}

type oaToolCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []oaToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type oaTool struct {
	Type     string     `json:"type"`
	Function oaFunction `json:"function"`
}

type oaRequest struct {
	Model               string      `json:"model"`
	Messages            []oaMessage `json:"messages"`
	Tools               []oaTool    `json:"tools,omitempty"`
	MaxTokens           int         `json:"max_tokens,omitempty"`
	MaxCompletionTokens int         `json:"max_completion_tokens,omitempty"`
	Temperature         *float64    `json:"temperature,omitempty"`
}

type oaResponse struct {
	Choices []struct {
		Message      oaMessage `json:"message"`
		FinishReason string    `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func (o *OpenAI) Chat(ctx context.Context, req Request) (*Response, error) {
	body := oaRequest{
		Model:       firstNonEmpty(req.Model, o.cfg.Model),
		Temperature: req.Temperature,
	}
	maxTok := firstPositive(req.MaxTokens, o.cfg.MaxTokens)
	if o.flavor == "openai" {
		body.MaxCompletionTokens = maxTok
	} else {
		body.MaxTokens = maxTok
	}
	if req.System != "" {
		body.Messages = append(body.Messages, oaMessage{Role: "system", Content: jsonString(req.System)})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			body.Messages = append(body.Messages, oaMessage{Role: "user", Content: o.userContent(m)})
		case RoleAssistant:
			am := oaMessage{Role: "assistant", Content: jsonString(m.Content)}
			for _, tc := range m.ToolCalls {
				args := string(argsOrEmpty(tc.Arguments))
				am.ToolCalls = append(am.ToolCalls, oaToolCall{ID: o.callID(tc.ID), Type: "function", Function: oaFunction{Name: tc.Name, Arguments: &args}})
			}
			body.Messages = append(body.Messages, am)
		case RoleTool:
			content := m.Content
			if m.IsError {
				content = "ERROR: " + content
			}
			body.Messages = append(body.Messages, oaMessage{Role: "tool", ToolCallID: o.callID(m.ToolCallID), Name: m.ToolName, Content: jsonString(content)})
		}
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, oaTool{Type: "function", Function: oaFunction{Name: t.Name, Description: t.Description, Parameters: schemaOrEmpty(t.Parameters)}})
	}

	headers := map[string]string{}
	endpoint := o.cfg.BaseURL + "/chat/completions"
	if o.cfg.APIVersion != "" { // Azure OpenAI
		endpoint += "?api-version=" + url.QueryEscape(o.cfg.APIVersion)
		headers["api-key"] = o.cfg.APIKey
	} else if o.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.cfg.APIKey
	}
	if o.cfg.Organization != "" {
		headers["OpenAI-Organization"] = o.cfg.Organization
	}
	for k, v := range o.cfg.Headers {
		headers[k] = v
	}

	var out oaResponse
	if err := o.http.postJSON(ctx, endpoint, headers, body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, &APIError{Provider: o.name, Status: 200, Body: "no choices in response"}
	}
	ch := out.Choices[0]
	msg := Message{Role: RoleAssistant, Content: decodeOAContent(ch.Message.Content)}
	for _, tc := range ch.Message.ToolCalls {
		args := ""
		if tc.Function.Arguments != nil {
			args = *tc.Function.Arguments
		}
		id := tc.ID
		if id == "" {
			id = newCallID()
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Arguments: argsOrEmpty(json.RawMessage(args))})
	}
	stop := ch.FinishReason
	switch stop {
	case "stop":
		stop = "end"
	case "length", "model_length":
		stop = "max_tokens"
	case "tool_calls", "function_call":
		stop = "tool_calls"
	}
	if len(msg.ToolCalls) > 0 {
		stop = "tool_calls"
	}
	return &Response{
		Message:    msg,
		StopReason: stop,
		Usage:      Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens},
	}, nil
}

// userContent is a plain string, or content parts when images ride along.
// Mistral documents image_url as a bare string, OpenAI (and vLLM) as {url}.
func (o *OpenAI) userContent(m Message) json.RawMessage {
	if len(m.Images) == 0 {
		return jsonString(m.Content)
	}
	parts := []map[string]any{}
	if m.Content != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, img := range m.Images {
		var u any = map[string]string{"url": img.DataURL()}
		if o.flavor == "mistral" {
			u = img.DataURL()
		}
		parts = append(parts, map[string]any{"type": "image_url", "image_url": u})
	}
	b, _ := json.Marshal(parts)
	return b
}

// decodeOAContent accepts a string or an array of content chunks.
func decodeOAContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var chunks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &chunks) == nil {
		var b strings.Builder
		for _, c := range chunks {
			if c.Type == "text" || c.Type == "" {
				b.WriteString(c.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

var mistralID = regexp.MustCompile(`^[a-zA-Z0-9]{9}$`)

// callID adapts tool call IDs to provider constraints. Mistral requires
// exactly 9 alphanumeric characters, which breaks when replaying history
// created by another provider.
func (o *OpenAI) callID(id string) string {
	if o.flavor != "mistral" || mistralID.MatchString(id) {
		return id
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	sum := sha256.Sum256([]byte(id))
	b := make([]byte, 9)
	for i := range b {
		b[i] = alphabet[int(sum[i])%len(alphabet)]
	}
	return string(b)
}
