// Package server exposes agents over HTTP: a native run API (JSON + SSE),
// an OpenAI-compatible chat completions API, an MCP endpoint, webhooks and,
// in multi-tenant mode, the self-service configuration and admin APIs.
package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/getitdone-gmbh/agentd/internal/agent"
	"github.com/getitdone-gmbh/agentd/internal/app"
	"github.com/getitdone-gmbh/agentd/internal/config"
	"github.com/getitdone-gmbh/agentd/internal/llm"
	"github.com/getitdone-gmbh/agentd/internal/mcp"
	"github.com/getitdone-gmbh/agentd/internal/metrics"
	"github.com/getitdone-gmbh/agentd/internal/store"
	"github.com/getitdone-gmbh/agentd/internal/tenant"
	"github.com/getitdone-gmbh/agentd/internal/tools"
)

// Version is set at build time.
var Version = "dev"

// Server is the HTTP front end.
type Server struct {
	m      *tenant.Manager
	cfg    *config.Config
	tokens [][]byte // admin tokens (server.auth_token)
	bg     context.Context
}

type ctxKey int

const (
	keyTenant ctxKey = iota
	keyAdmin
)

func tenantOf(r *http.Request) string {
	id, _ := r.Context().Value(keyTenant).(string)
	return id
}

func isAdmin(r *http.Request) bool {
	ok, _ := r.Context().Value(keyAdmin).(bool)
	return ok
}

// New builds the HTTP handler.
func New(bg context.Context, m *tenant.Manager, cfg *config.Config) http.Handler {
	s := &Server{m: m, cfg: cfg, bg: bg}
	for _, t := range cfg.Server.Tokens() {
		s.tokens = append(s.tokens, []byte(t))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		metrics.Write(w)
	})
	h := func(pattern string, fn http.HandlerFunc) { mux.Handle(pattern, s.auth(fn)) }
	h("GET /v1/agents", s.listAgents)
	h("GET /v1/agents/{name}", s.getAgent)
	h("POST /v1/agents/{name}/run", s.runAgent)
	h("DELETE /v1/agents/{name}/sessions/{id}", s.deleteSession)
	mux.HandleFunc("POST /v1/hooks/{name}", s.webhook)
	mux.HandleFunc("POST /v1/hooks/{name}/{hook}", s.webhook)
	mux.HandleFunc("POST /v1/tenants/{tenant}/hooks/{name}", s.webhook)
	mux.HandleFunc("POST /v1/tenants/{tenant}/hooks/{name}/{hook}", s.webhook)
	if cfg.Server.OpenAI == nil || *cfg.Server.OpenAI {
		h("GET /v1/models", s.listModels)
		h("POST /v1/chat/completions", s.chatCompletions)
	}
	if cfg.Server.MCP == nil || *cfg.Server.MCP {
		mux.Handle("/mcp", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := tenantOf(r)
			srv := &mcp.Server{Name: "agentd", Version: Version, Tools: func() []mcp.ServerTool { return s.mcpTools(r.Context(), id) }}
			srv.ServeHTTP(w, r)
		})))
	}
	if m.Enabled() {
		s.tenantRoutes(mux, h)
	}
	if cfg.Server.UI == nil || *cfg.Server.UI {
		uiRoutes(mux)
	}
	return s.cors(s.limitBody(mux))
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metrics.HTTPRequests.Add(1)
		// File uploads are capped by the tenant's per-file quota instead
		// (putFile), which may be larger than the API body limit.
		if !(r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/files/")) {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	// Entries may hold comma-separated lists (one env var for all origins).
	var origins []string
	for _, o := range s.cfg.Server.CORSOrigins {
		for _, part := range strings.Split(o, ",") {
			if part = strings.TrimSpace(part); part != "" {
				origins = append(origins, part)
			}
		}
	}
	if len(origins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		for _, o := range origins {
			if o == "*" || strings.EqualFold(o, origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, MCP-Protocol-Version, X-Tenant")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				break
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearer(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return r.Header.Get("X-API-Key")
}

func (s *Server) isAdminToken(tok string) bool {
	for _, t := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(tok), t) == 1 {
			return true
		}
	}
	return false
}

// resolve authenticates the request: admin tokens act on the system tenant
// (or the tenant named in X-Tenant); other tokens are tenant keys or external
// (OIDC userinfo) tokens.
func (s *Server) resolve(r *http.Request) (string, bool, error) {
	tok := bearer(r)
	if len(s.tokens) == 0 && !s.m.Enabled() {
		return tenant.System, true, nil // auth disabled (warned at startup)
	}
	if tok != "" && s.isAdminToken(tok) {
		if t := r.Header.Get("X-Tenant"); t != "" {
			return t, true, nil
		}
		return tenant.System, true, nil
	}
	id, err := s.m.Authenticate(r.Context(), tok)
	return id, false, err
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, admin, err := s.resolve(r)
		if err != nil {
			if !errors.Is(err, tenant.ErrUnauthorized) {
				slog.Warn("authentication failed", "path", r.URL.Path, "err", err)
				writeError(w, http.StatusServiceUnavailable, "authentication backend unavailable")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="agentd"`)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), keyTenant, id)
		ctx = context.WithValue(ctx, keyAdmin, admin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": msg}})
}

// fail maps errors to HTTP statuses.
func fail(w http.ResponseWriter, err error) {
	var ae *llm.APIError
	var ve *tenant.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusUnprocessableEntity, ve.Msg)
	case errors.Is(err, store.ErrFileQuota):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "already exists")
	case errors.Is(err, tenant.ErrReadOnly):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, tenant.ErrDisabled):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, tenant.ErrRateLimited):
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, tenant.ErrQuotaExceeded):
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.As(err, &ae):
		writeError(w, http.StatusBadGateway, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, 499, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// exposedAgent checks that name is an exposed agent of the tenant.
func (s *Server) exposedAgent(ctx context.Context, id, name string) (*agent.Agent, error) {
	a, release, err := s.m.Acquire(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	ag, ok := a.Agents[name]
	if !ok || !ag.Config().Exposed() {
		return nil, store.ErrNotFound
	}
	return ag, nil
}

type agentInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Provider    string   `json:"provider"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools"`
	Triggers    []string `json:"triggers,omitempty"`
}

func info(ag *agent.Agent) agentInfo {
	c := ag.Config()
	i := agentInfo{Name: c.Name, Description: c.Description, Provider: c.Provider, Model: c.Model, Tools: []string{}}
	for _, t := range ag.Tools() {
		i.Tools = append(i.Tools, t.Name)
	}
	return i
}

func exposedSorted(a *app.App) []*agent.Agent {
	var out []*agent.Agent
	for _, ag := range a.Agents {
		if ag.Config().Exposed() {
			out = append(out, ag)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Config().Name < out[j].Config().Name })
	return out
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	a, release, err := s.m.Acquire(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	defer release()
	list := []agentInfo{}
	for _, ag := range exposedSorted(a) {
		i := info(ag)
		for _, t := range a.Config.Agents[ag.Config().Name].Triggers {
			i.Triggers = append(i.Triggers, t.Type)
		}
		list = append(list, i)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	ag, err := s.exposedAgent(r.Context(), tenantOf(r), r.PathValue("name"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info(ag))
}

type runRequest struct {
	Input     string `json:"input"`
	SessionID string `json:"session_id"`
	Stream    bool   `json:"stream"`
}

func (s *Server) runAgent(w http.ResponseWriter, r *http.Request) {
	id, name := tenantOf(r), r.PathValue("name")
	if _, err := s.exposedAgent(r.Context(), id, name); err != nil {
		fail(w, err)
		return
	}
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}
	in := agent.RunInput{Input: req.Input, SessionID: req.SessionID}
	if req.Stream || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.streamRun(w, r, id, name, in)
		return
	}
	res, err := s.m.Run(r.Context(), id, name, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// streamRun sends agent events as server-sent events.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, id, name string, in agent.RunInput) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	events := make(chan agent.Event, 64)
	in.OnEvent = func(e agent.Event) {
		select {
		case events <- e:
		case <-r.Context().Done():
		}
	}
	type outcome struct {
		res *agent.RunResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := s.m.Run(r.Context(), id, name, in)
		done <- outcome{res, err}
	}()
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e := <-events:
			if e.Type != "done" {
				send(e.Type, e)
			}
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
			flusher.Flush()
		case o := <-done:
			for len(events) > 0 {
				if e := <-events; e.Type != "done" {
					send(e.Type, e)
				}
			}
			if o.err != nil {
				send("error", map[string]string{"error": o.err.Error()})
			} else {
				send("result", o.res)
			}
			return
		}
	}
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	a, release, err := s.m.Acquire(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	defer release()
	if _, ok := a.Agents[r.PathValue("name")]; !ok {
		fail(w, store.ErrNotFound)
		return
	}
	if err := a.Store.Delete(r.Context(), r.PathValue("name"), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- webhooks ----------

func (s *Server) findHook(ctx context.Context, tenantID, agentName, hook string) (config.Trigger, bool) {
	cfg, err := s.m.TenantConfig(ctx, tenantID)
	if err != nil {
		return config.Trigger{}, false
	}
	ac, ok := cfg.Agents[agentName]
	if !ok {
		return config.Trigger{}, false
	}
	for _, t := range ac.Triggers {
		if t.Type == "webhook" && t.Name == hook {
			return t, true
		}
	}
	return config.Trigger{}, false
}

func (s *Server) verifyHook(t config.Trigger, r *http.Request, body []byte, system bool) bool {
	adminOK := system && len(s.tokens) > 0 && s.isAdminToken(bearer(r))
	if t.Secret == "" {
		return adminOK
	}
	secret := []byte(t.Secret)
	if sig := r.Header.Get("X-Hub-Signature-256"); sig != "" { // GitHub / Gitea style HMAC
		mac := hmac.New(sha256.New, secret)
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(want))
	}
	got := r.Header.Get("X-Webhook-Secret")
	if got == "" && system {
		// Only the operator's own hooks: a secret in the URL ends up in
		// proxy and ingress logs, which on a shared platform are not the
		// tenant's.
		got = r.URL.Query().Get("secret")
	}
	return subtle.ConstantTimeCompare([]byte(got), secret) == 1 || adminOK
}

func (s *Server) webhook(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant")
	if tenantID == "" {
		tenantID = tenant.System
	}
	name := r.PathValue("name")
	if tenantID != tenant.System && !s.m.AllowHook(tenantID) {
		writeError(w, http.StatusTooManyRequests, "too many webhook calls, try again later")
		return
	}
	t, ok := s.findHook(r.Context(), tenantID, name, r.PathValue("hook"))
	if !ok {
		writeError(w, http.StatusNotFound, "webhook not found")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "body too large")
		return
	}
	if !s.verifyHook(t, r, body, tenantID == tenant.System) {
		writeError(w, http.StatusUnauthorized, "invalid webhook secret")
		return
	}
	payload := string(body)
	if len(payload) > 50000 {
		payload = payload[:50000] + "\n…(truncated)"
	}
	prompt := t.Prompt
	if prompt == "" {
		prompt = "A webhook was received. Handle it according to your instructions."
	}
	var b strings.Builder
	b.WriteString(prompt)
	if ev := r.Header.Get("X-GitHub-Event"); ev != "" {
		b.WriteString("\n\nEvent: " + ev)
	}
	b.WriteString("\n\nContent-Type: " + r.Header.Get("Content-Type") + "\nPayload:\n" + payload)
	in := agent.RunInput{Input: b.String(), SessionID: t.Session}
	metrics.TriggerRuns.Add(1)
	if t.Async {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("async webhook run panicked", "tenant", tenantID, "agent", name, "panic", r)
				}
			}()
			if _, err := s.m.Run(s.bg, tenantID, name, in); err != nil {
				_ = err // logged by the agent / manager
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
		return
	}
	res, err := s.m.Run(r.Context(), tenantID, name, in)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------- MCP ----------

func (s *Server) mcpTools(ctx context.Context, id string) []mcp.ServerTool {
	a, release, err := s.m.Acquire(ctx, id)
	if err != nil {
		return nil
	}
	defer release()
	var out []mcp.ServerTool
	for _, ag := range exposedSorted(a) {
		name := ag.Config().Name
		desc := ag.Config().Description
		if desc == "" {
			desc = "Runs the " + name + " agent."
		}
		out = append(out, mcp.ServerTool{
			Name:        name,
			Description: desc + " Returns the agent's answer. Pass session_id from a previous answer to continue the conversation.",
			InputSchema: tools.Schema(map[string]any{
				"input":      tools.Prop("string", "Task or question for the agent"),
				"session_id": tools.Prop("string", "Optional session to continue"),
			}, "input"),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Input     string `json:"input"`
					SessionID string `json:"session_id"`
				}
				if err := tools.Decode(args, &a); err != nil {
					return "", err
				}
				res, err := s.m.Run(ctx, id, name, agent.RunInput{Input: a.Input, SessionID: a.SessionID})
				if err != nil {
					return "", err
				}
				return res.Output + "\n\n[session_id: " + res.SessionID + "]", nil
			},
		})
	}
	return out
}
