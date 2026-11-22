package apiserver

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"autochrome/internal/envdir"
)

func (s *Server) validateChrome(version string) error {
	v := strings.TrimSpace(version)
	if v == "" || v == "default" || v == "system" {
		return nil
	}
	if s.Chrome == nil {
		return fmt.Errorf("chrome manager unavailable")
	}
	_, err := s.Chrome.Get(v)
	return err
}

func (s *Server) handleChrome(w http.ResponseWriter, _ *http.Request) {
	cur, err := s.Chrome.Current()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chrome_current_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": cur, "dir": s.Chrome.Dir()})
}

func (s *Server) handleChromeVersions(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Chrome.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "chrome_list_failed", err.Error())
		return
	}
	cur, _ := s.Chrome.Current()
	writeJSON(w, http.StatusOK, map[string]any{"current": cur, "versions": list, "dir": s.Chrome.Dir()})
}

func (s *Server) handleChromeUpdate(w http.ResponseWriter, _ *http.Request) {
	v, err := s.Chrome.Update()
	if err != nil {
		writeError(w, http.StatusBadGateway, "chrome_update_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleChromeInstall(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 512<<20)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_upload", err.Error())
			return
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_upload", "file field is required")
			return
		}
		defer file.Close()
		tmp, err := os.CreateTemp("", "autochrome-chrome-*.deb")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "chrome_install_failed", err.Error())
			return
		}
		name := tmp.Name()
		defer os.Remove(name)
		if _, err := io.Copy(tmp, file); err != nil {
			tmp.Close()
			writeError(w, http.StatusBadRequest, "chrome_install_failed", err.Error())
			return
		}
		if err := tmp.Close(); err != nil {
			writeError(w, http.StatusInternalServerError, "chrome_install_failed", err.Error())
			return
		}
		v, err := s.Chrome.InstallDeb(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "chrome_install_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, v)
		return
	}

	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.URL == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "url is required")
		return
	}
	v, err := s.Chrome.InstallURL(in.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "chrome_install_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleChromeUse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	v, err := s.Chrome.Use(in.Version)
	if err != nil {
		writeError(w, http.StatusBadRequest, "chrome_use_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleChromeDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Chrome.Remove(r.PathValue("version")); err != nil {
		writeError(w, http.StatusBadRequest, "chrome_delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleUserChrome(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.validateChrome(in.Version); err != nil {
		writeError(w, http.StatusBadRequest, "chrome_version_invalid", err.Error())
		return
	}
	s.userMu.Lock()
	old := s.User.ChromeVersion
	s.User.ChromeVersion = strings.TrimSpace(in.Version)
	if s.User.ChromeVersion == "default" {
		s.User.ChromeVersion = ""
	}
	err := s.User.Save()
	u := s.User
	if err != nil {
		s.User.ChromeVersion = old
	}
	s.userMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user_save_failed", err.Error())
		return
	}
	s.Manager.SetUser(u)
	if s.MCP != nil {
		s.MCP.SetUser(u)
	}
	if s.Jobs != nil {
		s.Jobs.SetUser(u)
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleUserChromeUnbind(w http.ResponseWriter, _ *http.Request) {
	s.userMu.Lock()
	old := s.User.ChromeVersion
	s.User.ChromeVersion = ""
	err := s.User.Save()
	u := s.User
	if err != nil {
		s.User.ChromeVersion = old
	}
	s.userMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user_save_failed", err.Error())
		return
	}
	s.Manager.SetUser(u)
	if s.MCP != nil {
		s.MCP.SetUser(u)
	}
	if s.Jobs != nil {
		s.Jobs.SetUser(u)
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) handleEnvChrome(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Version string `json:"version"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.validateChrome(in.Version); err != nil {
		writeError(w, http.StatusBadRequest, "chrome_version_invalid", err.Error())
		return
	}
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	version := strings.TrimSpace(in.Version)
	if version == "default" {
		version = ""
	}
	updated, err := envdir.SetChromeDir(dir, meta, version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_chrome_failed", err.Error())
		return
	}
	_ = s.Registry.Put(updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleEnvChromeUnbind(w http.ResponseWriter, r *http.Request) {
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	updated, err := envdir.SetChromeDir(dir, meta, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_chrome_failed", err.Error())
		return
	}
	_ = s.Registry.Put(updated)
	writeJSON(w, http.StatusOK, updated)
}
