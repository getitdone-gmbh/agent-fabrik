package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// ServerTool is a tool served over MCP.
type ServerTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server is a stateless streamable-HTTP MCP server (JSON responses only).
type Server struct {
	Name    string
	Version string
	Tools   func() []ServerTool
}

var supportedVersions = map[string]bool{"2024-11-05": true, "2025-03-26": true, "2025-06-18": true, "2025-11-25": true}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK) // stateless: nothing to tear down
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var m rpcMessage
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&m); err != nil {
		writeRPC(w, nil, nil, &RPCError{Code: -32700, Message: "parse error"})
		return
	}
	if len(m.ID) == 0 { // notification or response
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := ProtocolVersion
		if supportedVersions[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		writeRPC(w, m.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": s.Name, "version": s.Version},
		}, nil)
	case "ping":
		writeRPC(w, m.ID, map[string]any{}, nil)
	case "tools/list":
		list := []map[string]any{}
		for _, t := range s.Tools() {
			list = append(list, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		writeRPC(w, m.ID, map[string]any{"tools": list}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			writeRPC(w, m.ID, nil, &RPCError{Code: -32602, Message: "invalid params"})
			return
		}
		for _, t := range s.Tools() {
			if t.Name != p.Name {
				continue
			}
			text, err := t.Handler(r.Context(), p.Arguments)
			isErr := false
			if err != nil {
				text, isErr = err.Error(), true
			}
			writeRPC(w, m.ID, map[string]any{"content": []Content{{Type: "text", Text: text}}, "isError": isErr}, nil)
			return
		}
		writeRPC(w, m.ID, nil, &RPCError{Code: -32602, Message: "unknown tool: " + p.Name})
	default:
		writeRPC(w, m.ID, nil, &RPCError{Code: -32601, Message: "method not found: " + m.Method})
	}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *RPCError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		resp["id"] = nil
	}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
