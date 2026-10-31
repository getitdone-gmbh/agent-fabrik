package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/getitdone-gmbh/agent-fabrik/internal/store"
)

// The run log: what each agent was asked, which tools it called and what
// it answered. A run key (the one a website backend holds) cannot read it:
// the log holds other visitors' conversations.

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{Agent: q.Get("agent"), Status: q.Get("status")}
	switch f.Status {
	case "", "failed", store.RunOK, store.RunMaxSteps, store.RunError:
	default:
		writeError(w, http.StatusBadRequest, "status must be ok, max_steps, error or failed")
		return
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "limit must be a number")
			return
		}
		f.Limit = n
	}
	if v := q.Get("before"); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "before must be an RFC 3339 time")
			return
		}
		f.Before = t
	}
	id := tenantOf(r)
	if t := q.Get("tenant"); t != "" {
		if !isAdmin(r) {
			writeError(w, http.StatusForbidden, "admin token required")
			return
		}
		if t == "*" {
			f.AllTenants = true
		} else {
			id = t
		}
	}
	runs, err := s.m.Runs(r.Context(), id, f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	id := tenantOf(r)
	if t := r.URL.Query().Get("tenant"); t != "" {
		if !isAdmin(r) {
			writeError(w, http.StatusForbidden, "admin token required")
			return
		}
		id = t
	}
	run, err := s.m.GetRun(r.Context(), id, r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
