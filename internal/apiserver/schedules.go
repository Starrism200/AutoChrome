package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"autochrome/internal/jsruntime"
	"autochrome/internal/scheduler"
	"autochrome/internal/scripts"
)

type scheduleInput struct {
	Name            string         `json:"name"`
	Type            string         `json:"type"`
	Env             string         `json:"env"`
	URL             string         `json:"url"`
	ProxyID         string         `json:"proxy_id"`
	ChromeVersion   string         `json:"chrome_version"`
	Script          string         `json:"script"`
	Input           map[string]any `json:"input"`
	IntervalSeconds int            `json:"interval_seconds"`
	Enabled         *bool          `json:"enabled"`
}

func validateSchedule(j scheduler.Job) error {
	switch j.Type {
	case "script":
		if strings.TrimSpace(j.Script) == "" {
			return fmt.Errorf("script is required for type=script")
		}
	case "open":
		if strings.TrimSpace(j.Env) == "" {
			return fmt.Errorf("env is required for type=open")
		}
	case "close":
	default:
		return fmt.Errorf("type must be script, open or close")
	}
	if j.IntervalSeconds < 5 {
		return fmt.Errorf("interval_seconds must be >= 5")
	}
	return nil
}

func (s *Server) handleScheduleList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Schedules.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "schedule_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": list})
}

func (s *Server) handleScheduleGet(w http.ResponseWriter, r *http.Request) {
	j, err := s.Schedules.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "schedule_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) handleScheduleAdd(w http.ResponseWriter, r *http.Request) {
	var in scheduleInput
	if !decode(w, r, &in) {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	j := scheduler.Job{
		Name: in.Name, Type: in.Type, Env: in.Env, URL: in.URL,
		ProxyID: in.ProxyID, ChromeVersion: in.ChromeVersion,
		Script: in.Script, Input: in.Input, IntervalSeconds: in.IntervalSeconds,
		Enabled: enabled,
	}
	if err := validateSchedule(j); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_schedule", err.Error())
		return
	}
	created, err := s.Schedules.Add(j)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "schedule_create_failed", err.Error())
		return
	}
	s.publish("schedule.created", created)
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            *string         `json:"name"`
		Type            *string         `json:"type"`
		Env             *string         `json:"env"`
		URL             *string         `json:"url"`
		ProxyID         *string         `json:"proxy_id"`
		ChromeVersion   *string         `json:"chrome_version"`
		Script          *string         `json:"script"`
		Input           *map[string]any `json:"input"`
		IntervalSeconds *int            `json:"interval_seconds"`
		Enabled         *bool           `json:"enabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	updated, err := s.Schedules.Update(r.PathValue("id"), func(j *scheduler.Job) error {
		if in.Name != nil {
			j.Name = *in.Name
		}
		if in.Type != nil {
			j.Type = *in.Type
		}
		if in.Env != nil {
			j.Env = *in.Env
		}
		if in.URL != nil {
			j.URL = *in.URL
		}
		if in.ProxyID != nil {
			j.ProxyID = *in.ProxyID
		}
		if in.ChromeVersion != nil {
			j.ChromeVersion = *in.ChromeVersion
		}
		if in.Script != nil {
			j.Script = *in.Script
		}
		if in.Input != nil {
			j.Input = *in.Input
		}
		if in.IntervalSeconds != nil {
			j.IntervalSeconds = *in.IntervalSeconds
		}
		if in.Enabled != nil {
			j.Enabled = *in.Enabled
		}
		return validateSchedule(*j)
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "schedule_update_failed", err.Error())
		return
	}
	s.publish("schedule.updated", updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleScheduleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Schedules.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "schedule_not_found", err.Error())
		return
	}
	s.publish("schedule.deleted", map[string]any{"id": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleScheduleRun(w http.ResponseWriter, r *http.Request) {
	j, err := s.Schedules.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "schedule_not_found", err.Error())
		return
	}
	result, runErr := s.executeJob(j)
	status, msg := "ok", ""
	if runErr != nil {
		status, msg = "error", runErr.Error()
	}
	next := time.Now().Add(time.Duration(j.IntervalSeconds) * time.Second)
	_, _ = s.Schedules.MarkRun(j.ID, status, msg, next)
	s.publish("schedule.run", map[string]any{"id": j.ID, "type": j.Type, "status": status, "error": msg})
	writeJSON(w, http.StatusOK, map[string]any{"status": status, "error": msg, "result": result})
}

func (s *Server) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jobs, err := s.Schedules.Due(time.Now())
			if err != nil {
				continue
			}
			for _, j := range jobs {
				result, runErr := s.executeJob(j)
				status, msg := "ok", ""
				if runErr != nil {
					status, msg = "error", runErr.Error()
				}
				next := time.Now().Add(time.Duration(j.IntervalSeconds) * time.Second)
				_, _ = s.Schedules.MarkRun(j.ID, status, msg, next)
				s.publish("schedule.run", map[string]any{
					"id": j.ID, "type": j.Type, "status": status, "error": msg, "result": result,
				})
			}
		}
	}
}

func (s *Server) executeJob(j scheduler.Job) (any, error) {
	switch j.Type {
	case "script":
		script, src, err := scripts.Load(s.User.ScriptRoot, j.Script)
		if err != nil {
			return nil, err
		}
		runner := jsruntime.New(s.User, s.Manager, os.Stderr)
		runner.AllowEval = s.AllowEval
		runner.AutoClose = true
		runner.Timeout = script.TimeoutDuration()
		input := j.Input
		if input == nil {
			input = map[string]any{}
		}
		return runner.Run(src, input)
	case "open":
		if sess, ok := s.Manager.SessionForEnv(j.Env); ok {
			return map[string]any{"session_id": sess.ID, "already_open": true}, nil
		}
		proxyURL := ""
		if j.ProxyID != "" {
			p, err := s.Proxies.Get(j.ProxyID)
			if err != nil {
				return nil, err
			}
			if !p.Enabled {
				return nil, fmt.Errorf("proxy is disabled")
			}
			proxyURL = p.URL
		}
		sess, err := s.Manager.Open(j.Env, "", j.URL, proxyURL, false, j.ChromeVersion)
		if err != nil {
			return nil, err
		}
		return map[string]any{"session_id": sess.ID}, nil
	case "close":
		if j.Env != "" {
			s.Manager.CloseEnv(j.Env)
		} else {
			s.Manager.CloseAll()
		}
		return map[string]any{"closed": true}, nil
	default:
		return nil, fmt.Errorf("unknown schedule type %q", j.Type)
	}
}
