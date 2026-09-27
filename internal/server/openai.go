package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/agent"
	"github.com/getitdone-gmbh/agentd/internal/llm"
)

// OpenAI-compatible surface: any OpenAI client can talk to an agent by using
// the agent name as the model. Tool calls happen inside the runtime; the
// client only sees the final answer.

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	a, release, err := s.m.Acquire(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	defer release()
	data := []map[string]any{}
	for _, ag := range exposedSorted(a) {
		data = append(data, map[string]any{"id": ag.Config().Name, "object": "model", "owned_by": "agentd", "created": 0})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

type chatRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream bool   `json:"stream"`
	User   string `json:"user"`
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	name := strings.TrimPrefix(req.Model, "agent:")
	if _, err := s.exposedAgent(r.Context(), tenantOf(r), name); err != nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found; use an agent name", req.Model))
		return
	}
	// The last user message is the input; earlier user/assistant turns are
	// passed as history. Client system messages are added as context.
	var history []llm.Message
	var system []string
	input := ""
	last := -1
	for i, m := range req.Messages {
		if m.Role == "user" {
			last = i
		}
	}
	if last < 0 {
		writeError(w, http.StatusBadRequest, "at least one user message is required")
		return
	}
	for i, m := range req.Messages {
		text := contentText(m.Content)
		switch {
		case i == last:
			input = text
		case m.Role == "system" || m.Role == "developer":
			system = append(system, text)
		case m.Role == "user":
			history = append(history, llm.Message{Role: llm.RoleUser, Content: text})
		case m.Role == "assistant" && text != "":
			history = append(history, llm.Message{Role: llm.RoleAssistant, Content: text})
		}
	}
	if len(system) > 0 {
		input = "Context from the caller:\n" + strings.Join(system, "\n") + "\n\n" + input
	}
	if history == nil {
		history = []llm.Message{}
	}

	id := "chatcmpl-" + agent.NewSessionID()[2:]
	created := time.Now().Unix()
	res, err := s.m.Run(r.Context(), tenantOf(r), name, agent.RunInput{Input: input, History: history})
	if err != nil {
		fail(w, err)
		return
	}
	finish := "stop"
	if res.StopReason == "max_tokens" {
		finish = "length"
	}
	usage := map[string]int{"prompt_tokens": res.Usage.InputTokens, "completion_tokens": res.Usage.OutputTokens, "total_tokens": res.Usage.InputTokens + res.Usage.OutputTokens}

	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": name,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": res.Output}, "finish_reason": finish}},
			"usage":   usage,
		})
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	chunk := func(delta map[string]string, finishReason any) {
		b, _ := json.Marshal(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": name,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finishReason}},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	chunk(map[string]string{"role": "assistant"}, nil)
	chunk(map[string]string{"content": res.Output}, nil)
	chunk(map[string]string{}, finish)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
