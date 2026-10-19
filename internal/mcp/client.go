// Package mcp implements a Model Context Protocol client (streamable HTTP and
// stdio transports) and a stateless streamable-HTTP server.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProtocolVersion is the MCP revision this implementation speaks.
const ProtocolVersion = "2025-06-18"

// ErrClosed is returned when the transport is no longer usable.
var ErrClosed = errors.New("mcp: transport closed")

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  any             `json:"params,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("mcp error %d: %s", e.Code, e.Message) }

// transport sends JSON-RPC calls and notifications.
type transport interface {
	call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close() error
}

// ToolDef is a tool advertised by an MCP server.
type ToolDef struct {
	Name        string          `json:"name"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Content is one item of a tool result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Resource *struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType,omitempty"`
		Text     string `json:"text,omitempty"`
	} `json:"resource,omitempty"`
}

// CallResult is the result of tools/call.
type CallResult struct {
	Content           []Content       `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// Text flattens the result into model-readable text.
func (r *CallResult) Text() string {
	var parts []string
	for _, c := range r.Content {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "audio":
			parts = append(parts, fmt.Sprintf("[%s %s, %d bytes base64]", c.Type, c.MimeType, len(c.Data)))
		case "resource":
			if c.Resource != nil {
				if c.Resource.Text != "" {
					parts = append(parts, c.Resource.Text)
				} else {
					parts = append(parts, "[resource "+c.Resource.URI+"]")
				}
			}
		case "resource_link":
			parts = append(parts, "[resource link "+c.Name+" "+c.URI+"]")
		}
	}
	if len(parts) == 0 && len(r.StructuredContent) > 0 {
		return string(r.StructuredContent)
	}
	return strings.Join(parts, "\n")
}

// Client is an MCP client session.
type Client struct {
	t          transport
	nextID     atomic.Int64
	ServerName string
}

// ClientInfo identifies this runtime to servers.
var ClientInfo = map[string]string{"name": "agent-fabrik", "version": "1.0.0"}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	res, err := c.t.call(ctx, c.nextID.Add(1), method, params)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(res, out)
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) error {
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      ClientInfo,
	}, &res)
	if err != nil {
		return err
	}
	c.ServerName = res.ServerInfo.Name
	if h, ok := c.t.(*httpTransport); ok {
		h.setProtocol(res.ProtocolVersion)
	}
	return c.t.notify(ctx, "notifications/initialized", nil)
}

// ListTools returns all tools, following pagination.
func (c *Client) ListTools(ctx context.Context) ([]ToolDef, error) {
	var all []ToolDef
	cursor := ""
	for i := 0; i < 50; i++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var res struct {
			Tools      []ToolDef `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// CallTool invokes a tool.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	var res CallResult
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// Close shuts the transport down.
func (c *Client) Close() error { return c.t.close() }

// ---------- streamable HTTP transport ----------

type httpTransport struct {
	url     string
	headers map[string]string
	client  *http.Client

	mu        sync.Mutex
	sessionID string
	protocol  string
}

// NewHTTPClient creates a client for a streamable-HTTP MCP server.
// rt may be nil for the default transport.
func NewHTTPClient(url string, headers map[string]string, timeout time.Duration, rt http.RoundTripper) *Client {
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	client := &http.Client{
		Timeout:   timeout,
		Transport: rt,
		// headers usually carry the server's credentials; Go would re-send
		// custom ones (X-Api-Key, ...) to any redirect target.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("redirect to %s://%s is not allowed", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}
	return &Client{t: &httpTransport{url: url, headers: headers, client: client}}
}

func (h *httpTransport) setProtocol(v string) {
	h.mu.Lock()
	h.protocol = v
	h.mu.Unlock()
}

func (h *httpTransport) post(ctx context.Context, msg rpcRequest) (*http.Response, error) {
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	h.mu.Lock()
	if h.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", h.sessionID)
	}
	if h.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", h.protocol)
	}
	h.mu.Unlock()
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.sessionID = sid
		h.mu.Unlock()
	}
	return resp, nil
}

func (h *httpTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	rid := json.RawMessage(strconv.FormatInt(id, 10))
	resp, err := h.post(ctx, rpcRequest{JSONRPC: "2.0", ID: rid, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("mcp %s: HTTP %d: %s", method, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return readSSEResponse(resp.Body, rid)
	}
	var m rpcMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("mcp %s: decode: %w", method, err)
	}
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

// readSSEResponse reads server-sent events until the response for id arrives.
func readSSEResponse(r io.Reader, id json.RawMessage) (json.RawMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 32<<20)
	var data strings.Builder
	dispatch := func() (json.RawMessage, bool, error) {
		defer data.Reset()
		if data.Len() == 0 {
			return nil, false, nil
		}
		var m rpcMessage
		if err := json.Unmarshal([]byte(data.String()), &m); err != nil {
			return nil, false, nil
		}
		if m.Method != "" || !bytes.Equal(bytes.TrimSpace(m.ID), id) {
			return nil, false, nil // notification or unrelated message
		}
		if m.Error != nil {
			return nil, true, m.Error
		}
		return m.Result, true, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if res, done, err := dispatch(); done {
				return res, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if res, done, err := dispatch(); done {
		return res, err
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("mcp: stream ended without a response")
}

func (h *httpTransport) notify(ctx context.Context, method string, params any) error {
	resp, err := h.post(ctx, rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("mcp %s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}

func (h *httpTransport) close() error {
	h.mu.Lock()
	sid := h.sessionID
	h.mu.Unlock()
	if sid == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Mcp-Session-Id", sid)
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	if resp, err := h.client.Do(req); err == nil {
		resp.Body.Close()
	}
	return nil
}

// ---------- stdio transport ----------

type stdioTransport struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	wmu   sync.Mutex

	mu      sync.Mutex
	pending map[string]chan rpcMessage
	done    chan struct{}
	err     error
}

// NewStdioClient starts an MCP server subprocess speaking newline-delimited
// JSON-RPC over stdin/stdout. env is its complete environment.
func NewStdioClient(command []string, env []string, dir string, log *slog.Logger) (*Client, error) {
	if len(command) == 0 {
		return nil, errors.New("mcp: empty command")
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcp: start %s: %w", command[0], err)
	}
	t := &stdioTransport{cmd: cmd, stdin: stdin, pending: map[string]chan rpcMessage{}, done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if log != nil {
				log.Debug("mcp stderr", "cmd", command[0], "line", sc.Text())
			}
		}
	}()
	go t.readLoop(stdout)
	return &Client{t: t}, nil
}

func (s *stdioTransport) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var m rpcMessage
			if json.Unmarshal(line, &m) == nil {
				s.handle(m)
			}
		}
		if err != nil {
			s.mu.Lock()
			s.err = ErrClosed
			for _, ch := range s.pending {
				close(ch)
			}
			s.pending = map[string]chan rpcMessage{}
			s.mu.Unlock()
			close(s.done)
			_ = s.cmd.Wait()
			return
		}
	}
}

func (s *stdioTransport) handle(m rpcMessage) {
	if m.Method != "" {
		if len(m.ID) > 0 { // server-to-client request
			resp := map[string]any{"jsonrpc": "2.0", "id": m.ID}
			if m.Method == "ping" {
				resp["result"] = map[string]any{}
			} else {
				resp["error"] = RPCError{Code: -32601, Message: "method not supported by client: " + m.Method}
			}
			_ = s.write(resp)
		}
		return
	}
	key := string(bytes.TrimSpace(m.ID))
	s.mu.Lock()
	ch := s.pending[key]
	delete(s.pending, key)
	s.mu.Unlock()
	if ch != nil {
		ch <- m
	}
}

func (s *stdioTransport) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

func (s *stdioTransport) call(ctx context.Context, id int64, method string, params any) (json.RawMessage, error) {
	key := strconv.FormatInt(id, 10)
	ch := make(chan rpcMessage, 1)
	s.mu.Lock()
	if s.err != nil {
		s.mu.Unlock()
		return nil, s.err
	}
	s.pending[key] = ch
	s.mu.Unlock()
	if err := s.write(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(key), Method: method, Params: params}); err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return nil, ErrClosed
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, ErrClosed
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		_ = s.write(rpcRequest{JSONRPC: "2.0", Method: "notifications/cancelled", Params: map[string]any{"requestId": id, "reason": "timeout"}})
		return nil, ctx.Err()
	}
}

func (s *stdioTransport) notify(_ context.Context, method string, params any) error {
	return s.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (s *stdioTransport) close() error {
	_ = s.stdin.Close()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
	}
	return nil
}
