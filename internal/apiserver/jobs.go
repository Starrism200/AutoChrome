package apiserver

import (
	"net/http"
)

func (s *Server) handleScriptJobStart(w http.ResponseWriter, r *http.Request) {
	if !s.AllowScripts {
		writeError(w, http.StatusForbidden, "scripts_disabled", "scripts are disabled for this user")
		return
	}
	if s.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_unavailable", "job manager is not ready")
		return
	}
	var in struct {
		Input     map[string]any `json:"input"`
		SessionID string         `json:"session_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	job, err := s.Jobs.Start(r.PathValue("name"), in.Input, in.SessionID)
	if err != nil {
		writeError(w, http.StatusNotFound, "job_start_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handleJobList(w http.ResponseWriter, _ *http.Request) {
	if s.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_unavailable", "job manager is not ready")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.Jobs.List()})
}

func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_unavailable", "job manager is not ready")
		return
	}
	job, ok := s.Jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "job_not_found", "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_unavailable", "job manager is not ready")
		return
	}
	logs, err := s.Jobs.Logs(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "job_not_found", err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(logs))
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	if s.Jobs == nil {
		writeError(w, http.StatusServiceUnavailable, "jobs_unavailable", "job manager is not ready")
		return
	}
	if err := s.Jobs.Cancel(r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadRequest, "job_cancel_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}
