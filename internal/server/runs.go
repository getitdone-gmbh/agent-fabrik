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
	f := store.RunFilter{Agent: q.Get("agent"), SessionID: q.Get("session"), Status: q.Get("status")}
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

// getStats aggregates the run log: totals, per day, per agent.
func (s *Server) getStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	days := 7
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 30 {
			writeError(w, http.StatusBadRequest, "days must be between 1 and 30")
			return
		}
		days = n
	}
	// Whole UTC days, so the first bar of a chart is not a partial day.
	now := time.Now().UTC()
	since := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1-days)
	st, err := s.m.RunStats(r.Context(), tenantOf(r), q.Get("agent"), since)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// listAlerts returns the problems the alert checker currently sees.
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("tenant") == "*"
	if all && !isAdmin(r) {
		writeError(w, http.StatusForbidden, "admin token required")
		return
	}
	alerts, err := s.m.Alerts(r.Context(), tenantOf(r), all)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts})
}

// listSessions lists conversations (runs grouped by session id).
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SessionFilter{Agent: q.Get("agent"), Failed: q.Get("status") == "failed"}
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
	sessions, err := s.m.Sessions(r.Context(), tenantOf(r), f)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}
