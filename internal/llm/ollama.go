package llm

import (
	"context"
	"encoding/json"
	"strings"
)

// Ollama speaks Ollama's native /api/chat endpoint.
type Ollama struct {
	name string
	cfg  Config
	http httpClient
}

func newOllama(name string, c Config) *Ollama {
	if c.BaseURL == "" {
		c.BaseURL = "http://localhost:11434"
	}
	c.BaseURL = strings.TrimSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1")
	if c.Model == "" {
		c.Model = "qwen3"
	}
	return &Ollama{name: name, cfg: c, http: newHTTPClient(name, c)}
}

func (o *Ollama) Name() string { return o.name }

type olToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type olMessage struct {
	Role      string       `json:"role"`
	Content   string       `json:"content"`
	Images    []string     `json:"images,omitempty"` // base64
	ToolCalls []olToolCall `json:"tool_calls,omitempty"`
	ToolName  string       `json:"tool_name,omitempty"`
}

type olRequest struct {
	Model     string         `json:"model"`
	Messages  []olMessage    `json:"messages"`
	Tools     []oaTool       `json:"tools,omitempty"`
	Stream    bool           `json:"stream"`
	KeepAlive string         `json:"keep_alive,omitempty"`
	Options   map[string]any `json:"options,omitempty"`
}

type olResponse struct {
	Message         olMessage `json:"message"`
	DoneReason      string    `json:"done_reason"`
	PromptEvalCount int       `json:"prompt_eval_count"`
	EvalCount       int       `json:"eval_count"`
}

func (o *Ollama) Chat(ctx context.Context, req Request) (*Response, error) {
	body := olRequest{Model: firstNonEmpty(req.Model, o.cfg.Model), KeepAlive: o.cfg.KeepAlive}
	opts := map[string]any{}
	if n := firstPositive(req.MaxTokens, o.cfg.MaxTokens); n > 0 {
		opts["num_predict"] = n
	}
	if o.cfg.ContextSize > 0 {
		opts["num_ctx"] = o.cfg.ContextSize
	}
	if req.Temperature != nil {
		opts["temperature"] = *req.Temperature
	}
	if len(opts) > 0 {
		body.Options = opts
	}
	if req.System != "" {
		body.Messages = append(body.Messages, olMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case RoleUser:
			um := olMessage{Role: "user", Content: m.Content}
			for _, img := range m.Images {
				um.Images = append(um.Images, img.Data)
			}
			body.Messages = append(body.Messages, um)
		case RoleAssistant:
			am := olMessage{Role: "assistant", Content: m.Content}
			for _, tc := range m.ToolCalls {
				var c olToolCall
				c.Function.Name = tc.Name
				c.Function.Arguments = argsOrEmpty(tc.Arguments)
				am.ToolCalls = append(am.ToolCalls, c)
			}
			body.Messages = append(body.Messages, am)
		case RoleTool:
			content := m.Content
			if m.IsError {
				content = "ERROR: " + content
			}
			body.Messages = append(body.Messages, olMessage{Role: "tool", Content: content, ToolName: m.ToolName})
		}
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, oaTool{Type: "function", Function: oaFunction{Name: t.Name, Description: t.Description, Parameters: schemaOrEmpty(t.Parameters)}})
	}

	headers := map[string]string{}
	if o.cfg.APIKey != "" {
		headers["Authorization"] = "Bearer " + o.cfg.APIKey
	}
	for k, v := range o.cfg.Headers {
		headers[k] = v
	}
	var out olResponse
	if err := o.http.postJSON(ctx, o.cfg.BaseURL+"/api/chat", headers, body, &out); err != nil {
		return nil, err
	}
	msg := Message{Role: RoleAssistant, Content: out.Message.Content}
	for _, tc := range out.Message.ToolCalls {
		args := tc.Function.Arguments
		// Some models return arguments as a JSON-encoded string.
		var s string
		if json.Unmarshal(args, &s) == nil {
			args = json.RawMessage(s)
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: newCallID(), Name: tc.Function.Name, Arguments: argsOrEmpty(args)})
	}
	stop := "end"
	if out.DoneReason == "length" {
		stop = "max_tokens"
	}
	if len(msg.ToolCalls) > 0 {
		stop = "tool_calls"
	}
	return &Response{
		Message:    msg,
		StopReason: stop,
		Usage:      Usage{InputTokens: out.PromptEvalCount, OutputTokens: out.EvalCount},
	}, nil
}
