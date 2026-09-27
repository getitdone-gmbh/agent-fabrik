// Package tools holds the tool registry and the protocol connectors
// (MCP, HTTP, SMTP, IMAP, builtins) that contribute tools to agents.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Handler executes a tool call and returns text for the model.
type Handler func(ctx context.Context, args json.RawMessage) (string, error)

// Tool is a callable capability exposed to models.
type Tool struct {
	Name        string // model-facing name: <connector>__<tool>
	Connector   string // connector name
	Local       string // tool name inside the connector
	Description string
	Schema      json.RawMessage // JSON Schema for arguments
	Handler     Handler
}

// Connector is a source of tools (an MCP server, an HTTP API, a mailbox, ...).
type Connector interface {
	Name() string
	Tools(ctx context.Context) ([]Tool, error)
	Close() error
}

var invalidName = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// QualifiedName builds a provider-safe tool name (^[a-zA-Z0-9_-]{1,64}$).
func QualifiedName(connector, local string) string {
	n := invalidName.ReplaceAllString(connector, "_") + "__" + invalidName.ReplaceAllString(local, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// Registry indexes tools from all connectors.
type Registry struct {
	mu         sync.RWMutex
	tools      map[string]Tool
	connectors map[string]Connector
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, connectors: map[string]Connector{}}
}

// Add registers a connector and loads its tools.
func (r *Registry) Add(ctx context.Context, c Connector) error {
	ts, err := c.Tools(ctx)
	if err != nil {
		return fmt.Errorf("connector %q: %w", c.Name(), err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.connectors[c.Name()] = c
	for _, t := range ts {
		t.Connector = c.Name()
		if t.Name == "" {
			t.Name = QualifiedName(c.Name(), t.Local)
		}
		if len(t.Schema) == 0 {
			t.Schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		r.tools[t.Name] = t
	}
	return nil
}

// Get returns a tool by model-facing name.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Select returns tools matching patterns like "mail.send_email", "github.*"
// or "*" (sorted by name for stable prompts and prompt caching).
func (r *Registry) Select(patterns []string) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Tool
	for _, t := range r.tools {
		id := t.Connector + "." + t.Local
		for _, p := range patterns {
			if ok, _ := path.Match(p, id); ok || p == t.Name || p == "*" {
				out = append(out, t)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// All returns every tool.
func (r *Registry) All() []Tool { return r.Select([]string{"*"}) }

// Close closes all connectors.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.connectors {
		_ = c.Close()
	}
}

// Decode unmarshals tool arguments with a helpful error.
func Decode(args json.RawMessage, v any) error {
	if len(strings.TrimSpace(string(args))) == 0 {
		args = json.RawMessage(`{}`)
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// JSON renders v for the model.
func JSON(v any) string {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// Schema builds a JSON object schema from property definitions.
func Schema(props map[string]any, required ...string) json.RawMessage {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	b, _ := json.Marshal(s)
	return b
}

// Prop builds a simple property schema.
func Prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}
