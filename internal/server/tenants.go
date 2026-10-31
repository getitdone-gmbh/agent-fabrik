package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"sort"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/getitdone-gmbh/agent-fabrik/internal/config"
	"github.com/getitdone-gmbh/agent-fabrik/internal/store"
	"github.com/getitdone-gmbh/agent-fabrik/internal/tenant"
)

// Self-service configuration API for tenants plus the admin API.
//
//   GET    /v1/me                         tenant, limits, usage
//   GET    /v1/config                     all definitions + secret names
//   PUT    /v1/config[?prune=true]        apply a YAML/JSON document
//   GET    /v1/config/{kind}/{name}       one definition (kind: providers, connectors, agents)
//   PUT    /v1/config/{kind}/{name}       create/replace
//   DELETE /v1/config/{kind}/{name}
//   GET    /v1/secrets                    names only
//   PUT    /v1/secrets/{name}             {"value": "..."}
//   DELETE /v1/secrets/{name}
//   GET    /v1/keys, POST /v1/keys {"name"}, DELETE /v1/keys/{id}
//   GET    /v1/files[?prefix=], GET/PUT/DELETE /v1/files/{path}   (raw bytes)
//   GET    /v1/runs[?agent=&status=&before=&limit=], GET /v1/runs/{id}   run log
//   GET    /v1/stats[?agent=&days=7]      totals, per day, per agent
//   GET    /v1/alerts                     problems the alert checker sees
//   admin: /v1/admin/tenants[/{id}[/keys]], /v1/admin/stats

var kindPaths = map[string]string{"providers": store.KindProvider, "connectors": store.KindConnector, "agents": store.KindAgent}

func (s *Server) tenantRoutes(mux *http.ServeMux, h func(string, http.HandlerFunc)) {
	h("GET /v1/me", s.me)
	h("GET /v1/config", s.getConfig)
	h("PUT /v1/config", s.putConfig)
	h("GET /v1/config/{kind}/{name}", s.getResource)
	h("PUT /v1/config/{kind}/{name}", s.putResource)
	h("DELETE /v1/config/{kind}/{name}", s.deleteResource)
	h("GET /v1/secrets", s.listSecrets)
	h("PUT /v1/secrets/{name}", s.putSecret)
	h("DELETE /v1/secrets/{name}", s.deleteSecret)
	h("GET /v1/keys", s.listKeys)
	h("POST /v1/keys", s.createKey)
	h("DELETE /v1/keys/{id}", s.deleteKey)
	h("GET /v1/files", s.listFiles)
	h("GET /v1/files/{path...}", s.getFile)
	h("PUT /v1/files/{path...}", s.putFile)
	h("DELETE /v1/files/{path...}", s.deleteFile)
	h("GET /v1/runs", s.listRuns)
	h("GET /v1/runs/{id}", s.getRun)
	h("GET /v1/stats", s.getStats)
	h("GET /v1/alerts", s.listAlerts)

	a := func(pattern string, fn http.HandlerFunc) { h(pattern, s.adminOnly(fn)) }
	a("GET /v1/admin/tenants", s.adminListTenants)
	a("POST /v1/admin/tenants", s.adminCreateTenant)
	a("GET /v1/admin/tenants/{id}", s.adminGetTenant)
	a("PATCH /v1/admin/tenants/{id}", s.adminPatchTenant)
	a("DELETE /v1/admin/tenants/{id}", s.adminDeleteTenant)
	a("POST /v1/admin/tenants/{id}/keys", s.adminCreateKey)
	a("GET /v1/admin/stats", s.adminStats)
}

func (s *Server) adminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAdmin(r) {
			writeError(w, http.StatusForbidden, "admin token required")
			return
		}
		next(w, r)
	}
}

// decodeDoc parses a YAML or JSON body into a generic value.
func decodeDoc(r *http.Request, v any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, v); err != nil {
		return &tenant.ValidationError{Msg: "invalid YAML/JSON: " + err.Error()}
	}
	return nil
}

func toJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, &tenant.ValidationError{Msg: "unsupported value: " + err.Error()}
	}
	return b, nil
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	id := tenantOf(r)
	t, err := s.m.Tenant(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	out := map[string]any{"tenant": t, "limits": s.m.Limits(t), "admin": isAdmin(r), "shared_providers": s.m.SharedProviders(t)}
	if id != tenant.System {
		if u, err := s.m.Usage(r.Context(), id); err == nil {
			out["usage"] = u
		}
		out["webhook_path"] = "/v1/tenants/" + id + "/hooks/{agent}/{name}"
		if f, err := s.m.Files(r.Context(), id); err == nil {
			if u, err := f.Usage(r.Context()); err == nil {
				q := f.Quota()
				out["files"] = map[string]int64{"files": u.Files, "bytes": u.Bytes,
					"max_files": q.MaxFiles, "max_file_bytes": q.MaxFileBytes, "max_total_bytes": q.MaxTotalBytes}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	id := tenantOf(r)
	res, err := s.m.Resources(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	names, err := s.m.SecretNames(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	doc := map[string]map[string]json.RawMessage{"providers": {}, "connectors": {}, "agents": {}}
	for _, rs := range res {
		doc[rs.Kind+"s"][rs.Name] = rs.Spec
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": doc["providers"], "connectors": doc["connectors"], "agents": doc["agents"], "secrets": names})
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var doc struct {
		Providers  map[string]any    `yaml:"providers"`
		Connectors map[string]any    `yaml:"connectors"`
		Agents     map[string]any    `yaml:"agents"`
		Secrets    map[string]string `yaml:"secrets"`
	}
	if err := decodeDoc(r, &doc); err != nil {
		fail(w, err)
		return
	}
	var mu tenant.Mutation
	add := func(kind string, m map[string]any) error {
		for name, spec := range m {
			b, err := toJSON(spec)
			if err != nil {
				return err
			}
			mu.Put = append(mu.Put, store.Resource{Kind: kind, Name: name, Spec: b})
		}
		return nil
	}
	for _, e := range []error{add(store.KindProvider, doc.Providers), add(store.KindConnector, doc.Connectors), add(store.KindAgent, doc.Agents)} {
		if e != nil {
			fail(w, e)
			return
		}
	}
	mu.PutSecrets = doc.Secrets
	mu.ReplaceAll = r.URL.Query().Get("prune") == "true"
	if err := s.m.Apply(r.Context(), tenantOf(r), mu); err != nil {
		fail(w, err)
		return
	}
	s.getConfig(w, r)
}

func kindOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	k, ok := kindPaths[r.PathValue("kind")]
	if !ok {
		writeError(w, http.StatusNotFound, "kind must be providers, connectors or agents")
	}
	return k, ok
}

func (s *Server) getResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	res, err := s.m.Resources(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	for _, rs := range res {
		if rs.Kind == kind && rs.Name == r.PathValue("name") {
			writeJSON(w, http.StatusOK, rs)
			return
		}
	}
	fail(w, store.ErrNotFound)
}

func (s *Server) putResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	var spec any
	if err := decodeDoc(r, &spec); err != nil {
		fail(w, err)
		return
	}
	if _, isMap := spec.(map[string]any); !isMap {
		writeError(w, http.StatusUnprocessableEntity, "body must be an object")
		return
	}
	b, err := toJSON(spec)
	if err != nil {
		fail(w, err)
		return
	}
	res := store.Resource{Kind: kind, Name: r.PathValue("name"), Spec: b}
	if err := s.m.Apply(r.Context(), tenantOf(r), tenant.Mutation{Put: []store.Resource{res}}); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) deleteResource(w http.ResponseWriter, r *http.Request) {
	kind, ok := kindOf(w, r)
	if !ok {
		return
	}
	id := tenantOf(r)
	res, err := s.m.Resources(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	found := false
	for _, rs := range res {
		found = found || (rs.Kind == kind && rs.Name == r.PathValue("name"))
	}
	if !found {
		fail(w, store.ErrNotFound)
		return
	}
	if err := s.m.Apply(r.Context(), id, tenant.Mutation{Delete: []store.Resource{{Kind: kind, Name: r.PathValue("name")}}}); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	names, err := s.m.SecretNames(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"secrets": names})
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value *string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Value == nil {
		writeError(w, http.StatusBadRequest, `body must be {"value": "..."}`)
		return
	}
	name := r.PathValue("name")
	if err := s.m.Apply(r.Context(), tenantOf(r), tenant.Mutation{PutSecrets: map[string]string{name: *body.Value}}); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	id := tenantOf(r)
	names, err := s.m.SecretNames(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	i := sort.SearchStrings(names, r.PathValue("name"))
	if i >= len(names) || names[i] != r.PathValue("name") {
		fail(w, store.ErrNotFound)
		return
	}
	if err := s.m.Apply(r.Context(), id, tenant.Mutation{DeleteSecrets: []string{r.PathValue("name")}}); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.m.Store().ListKeys(r.Context(), tenantOf(r))
	if err != nil {
		fail(w, err)
		return
	}
	if keys == nil {
		keys = []store.APIKey{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (s *Server) newKey(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Name   string   `json:"name"`
		Scope  string   `json:"scope"`
		Agents []string `json:"agents"`
	}
	// An empty body creates a default (admin) key; a body that does not
	// parse must not: {"scope":123} used to fall through to scope admin.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	plain, k, err := s.m.CreateKey(r.Context(), id, tenant.KeyOptions{Name: body.Name, Scope: body.Scope, Agents: body.Agents})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": plain, "info": k, "note": "store this key now; it is not shown again"})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) { s.newKey(w, r, tenantOf(r)) }

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteKey(r.Context(), tenantOf(r), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- admin ----------

func (s *Server) adminListTenants(w http.ResponseWriter, r *http.Request) {
	ts, err := s.m.Store().ListTenants(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if ts == nil {
		ts = []store.Tenant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": ts})
}

type tenantBody struct {
	ID       string         `json:"id"`
	Name     *string        `json:"name"`
	Plan     *string        `json:"plan"`
	Limits   *config.Limits `json:"limits"`
	Disabled *bool          `json:"disabled"`
	// ClearLimits removes a limits override (back to the plan).
	ClearLimits bool `json:"clear_limits"`
}

func (s *Server) adminCreateTenant(w http.ResponseWriter, r *http.Request) {
	var b tenantBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	t := store.Tenant{ID: b.ID, Limits: b.Limits}
	if b.Name != nil {
		t.Name = *b.Name
	}
	if b.Plan != nil {
		t.Plan = *b.Plan
	}
	created, err := s.m.CreateTenant(r.Context(), t)
	if err != nil {
		fail(w, err)
		return
	}
	plain, _, err := s.m.CreateKey(r.Context(), created.ID, tenant.KeyOptions{Name: "initial"})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tenant": created, "key": plain})
}

func (s *Server) adminGetTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.m.Store().GetTenant(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	u, _ := s.m.Usage(r.Context(), id)
	res, _ := s.m.Store().ListResources(r.Context(), id)
	counts := map[string]int{}
	for _, rs := range res {
		counts[rs.Kind+"s"]++
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenant": t, "limits": s.m.Limits(t), "usage": u, "counts": counts})
}

func (s *Server) adminPatchTenant(w http.ResponseWriter, r *http.Request) {
	t, err := s.m.Store().GetTenant(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	var b tenantBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if b.Name != nil {
		t.Name = *b.Name
	}
	if b.Plan != nil {
		t.Plan = *b.Plan
	}
	if b.Limits != nil {
		t.Limits = b.Limits
	}
	if b.ClearLimits {
		t.Limits = nil
	}
	if b.Disabled != nil {
		t.Disabled = *b.Disabled
	}
	if err := s.m.UpdateTenant(r.Context(), *t); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) adminDeleteTenant(w http.ResponseWriter, r *http.Request) {
	if err := s.m.DeleteTenant(r.Context(), r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	s.newKey(w, r, r.PathValue("id"))
}

func (s *Server) adminStats(w http.ResponseWriter, r *http.Request) {
	st := s.m.Stats()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st["heap_bytes"] = ms.HeapInuse
	st["goroutines"] = runtime.NumGoroutine()
	if ts, err := s.m.Store().TenantVersions(r.Context()); err == nil {
		st["tenants"] = len(ts)
	}
	st["version"] = Version
	st["leader"] = s.m.IsLeader()
	writeJSON(w, http.StatusOK, st)
}

// ---------- files ----------

func (s *Server) tenantFiles(w http.ResponseWriter, r *http.Request) (*tenant.Files, bool) {
	id := tenantOf(r)
	if id == tenant.System {
		writeError(w, http.StatusBadRequest, "files belong to a tenant; use a tenant key (or the admin token with X-Tenant)")
		return nil, false
	}
	f, err := s.m.Files(r.Context(), id)
	if err != nil {
		fail(w, err)
		return nil, false
	}
	return f, true
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	f, ok := s.tenantFiles(w, r)
	if !ok {
		return
	}
	list, err := f.ListFiles(r.Context(), r.URL.Query().Get("prefix"), 1000)
	if err != nil {
		fail(w, err)
		return
	}
	u, err := f.Usage(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	q := f.Quota()
	writeJSON(w, http.StatusOK, map[string]any{"files": list, "usage": u,
		"quota": map[string]int64{"max_files": q.MaxFiles, "max_file_bytes": q.MaxFileBytes, "max_total_bytes": q.MaxTotalBytes}})
}

// fileTransferTimeout bounds how long one upload or download may hold a
// file-transfer slot (25 MB at ~200 KB/s).
const fileTransferTimeout = 2 * time.Minute

func (s *Server) getFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.tenantFiles(w, r)
	if !ok {
		return
	}
	release, err := f.Hold(r.Context())
	if err != nil {
		return
	}
	defer release()
	// The slot is shared by all tenants: a client that never reads must not
	// keep it.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(fileTransferTimeout))
	defer rc.SetWriteDeadline(time.Time{}) // must not outlive this response on a keep-alive connection
	info, data, err := f.Get(r.Context(), r.PathValue("path"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", info.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	// Never render tenant content as a page on the API origin.
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(path.Base(info.Path)))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Last-Modified", info.UpdatedAt.UTC().Format(http.TimeFormat))
	_, _ = w.Write(data)
}

func (s *Server) putFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.tenantFiles(w, r)
	if !ok {
		return
	}
	limit := f.Quota().MaxFileBytes
	if limit <= 0 {
		limit = 1 << 30
	}
	if r.ContentLength > limit {
		// Refused before waiting for a slot or reading a byte.
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large: limit is %d bytes per file", limit))
		return
	}
	release, err := f.Hold(r.Context())
	if err != nil {
		return
	}
	defer release()
	// The slot is shared by all tenants: a client trickling its body must
	// not keep it.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(fileTransferTimeout))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large: limit is %d bytes per file", limit))
		return
	}
	ct := r.Header.Get("Content-Type")
	if ct == "application/x-www-form-urlencoded" { // curl's default for --data-binary
		ct = ""
	}
	info, err := f.Put(r.Context(), r.PathValue("path"), data, ct)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	f, ok := s.tenantFiles(w, r)
	if !ok {
		return
	}
	if err := f.Remove(r.Context(), r.PathValue("path")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
