// Package apiserver exposes AutoChrome as a local HTTP/JSON API. One server
// process is bound to one user, matching the L2 isolation model used by the
// MCP server.
package apiserver

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"autochrome/internal/browser"
	"autochrome/internal/desktop"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
	"autochrome/internal/jsruntime"
	"autochrome/internal/scripts"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

// Version is reported by /api/v1/health.
const Version = "0.3.0"

// Server is the HTTP API bound to one user.
type Server struct {
	User         users.User
	Chrome       string
	Addr         string
	Token        string
	AllowOrigin  string
	AllowEval    bool
	AllowScripts bool
	Desktop      *desktop.Desktop
	Manager      *session.Manager
}

// New returns a server for u.
func New(u users.User, chrome string) *Server {
	return &Server{
		User:         u,
		Chrome:       chrome,
		Addr:         "127.0.0.1:8787",
		AllowEval:    u.AllowEval,
		AllowScripts: u.AllowScripts,
	}
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.Manager = session.NewManager(s.User, s.Chrome)
	if s.Desktop != nil {
		s.Manager.SetDisplay(s.Desktop.Display())
	}
	defer s.Manager.CloseAll()

	mux := http.NewServeMux()
	s.routes(mux)
	handler := s.cors(s.auth(mux))
	srv := &http.Server{
		Addr:              s.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/user", s.handleUser)

	mux.HandleFunc("GET /api/v1/envs", s.handleEnvList)
	mux.HandleFunc("POST /api/v1/envs", s.handleEnvCreate)

	mux.HandleFunc("GET /api/v1/sessions", s.handleSessionList)
	mux.HandleFunc("POST /api/v1/sessions", s.handleSessionCreate)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleSessionGet)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleSessionDelete)

	mux.HandleFunc("POST /api/v1/sessions/{id}/navigate", s.handleNavigate)
	mux.HandleFunc("GET /api/v1/sessions/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /api/v1/sessions/{id}/click", s.handleClick)
	mux.HandleFunc("POST /api/v1/sessions/{id}/type", s.handleType)
	mux.HandleFunc("POST /api/v1/sessions/{id}/press", s.handlePress)
	mux.HandleFunc("POST /api/v1/sessions/{id}/wait", s.handleWait)
	mux.HandleFunc("POST /api/v1/sessions/{id}/eval", s.handleEval)
	mux.HandleFunc("GET /api/v1/sessions/{id}/screenshot", s.handleScreenshot)
	mux.HandleFunc("GET /api/v1/sessions/{id}/cookies", s.handleCookies)
	mux.HandleFunc("GET /api/v1/sessions/{id}/challenges", s.handleChallenges)
	mux.HandleFunc("GET /api/v1/sessions/{id}/qr", s.handleQR)
	mux.HandleFunc("POST /api/v1/sessions/{id}/click_xy", s.handleClickXY)
	mux.HandleFunc("POST /api/v1/sessions/{id}/scroll", s.handleScroll)

	mux.HandleFunc("GET /api/v1/scripts", s.handleScriptList)
	mux.HandleFunc("POST /api/v1/scripts/{name}/run", s.handleScriptRun)

	mux.HandleFunc("GET /api/v1/desktop", s.handleDesktop)
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	if s.AllowOrigin == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.AllowOrigin)
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("X-API-Token"))
}

// ---- health and user ----

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
}

func (s *Server) handleUser(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.User)
}

// ---- environments ----

func (s *Server) handleEnvList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"envs": s.listEnvs()})
}

func (s *Server) listEnvs() []envdir.Meta {
	out := []envdir.Meta{}
	entries, err := os.ReadDir(s.User.EnvRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if meta, ok := envdir.ReadMeta(filepath.Join(s.User.EnvRoot, e.Name())); ok {
			out = append(out, meta)
		}
	}
	return out
}

func (s *Server) handleEnvCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string `json:"id"`
		Platform string `json:"platform"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "id is required")
		return
	}
	if in.Platform == "" {
		in.Platform = "default"
	}
	fp := fingerprint.Generate(in.ID)
	dir, err := envdir.Create(s.User.EnvRoot, in.Platform, in.ID, fp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_create_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"dir": dir, "fingerprint": fp})
}

// ---- sessions ----

func (s *Server) handleSessionList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.Manager.List()})
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Env      string `json:"env"`
		Platform string `json:"platform"`
		URL      string `json:"url"`
		Proxy    string `json:"proxy"`
		Headless bool   `json:"headless"`
	}
	if !decode(w, r, &in) {
		return
	}
	sess, err := s.Manager.Open(in.Env, in.Platform, in.URL, in.Proxy, in.Headless)
	if err != nil {
		writeError(w, http.StatusBadRequest, "open_failed", err.Error())
		return
	}
	out := map[string]any{"session_id": sess.ID, "env": sess.EnvID}
	_ = sess.Do(func(ctx context.Context) error {
		out["url"], _ = browser.Location(ctx)
		out["title"], _ = browser.Title(ctx)
		return nil
	})
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.Info())
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Manager.Close(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": true})
}

// ---- page actions ----

func (s *Server) handleNavigate(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	var out struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	err := sess.Do(func(ctx context.Context) error {
		if err := browser.Navigate(ctx, in.URL); err != nil {
			return err
		}
		out.URL, _ = browser.Location(ctx)
		out.Title, _ = browser.Title(ctx)
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "navigate_failed", err.Error())
		return
	}
	sess.SetURL(out.URL)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	max, _ := strconv.Atoi(r.URL.Query().Get("max"))
	var snap browser.Snapshot
	err := sess.Do(func(ctx context.Context) error {
		v, e := browser.TakeSnapshot(ctx, max)
		snap = v
		return e
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "snapshot_failed", err.Error())
		return
	}
	sess.SetURL(snap.URL)
	writeJSON(w, http.StatusOK, map[string]any{
		"url": snap.URL, "title": snap.Title, "snapshot": browser.SnapshotText(snap), "nodes": snap.Nodes,
	})
}

func (s *Server) handleClick(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		Ref      string `json:"ref"`
		Selector string `json:"selector"`
		Text     string `json:"text"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := sess.Do(func(ctx context.Context) error {
		return browser.Click(ctx, browser.Target{Ref: in.Ref, Selector: in.Selector, Text: in.Text})
	}); err != nil {
		writeError(w, http.StatusBadGateway, "click_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleType(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		Ref      string `json:"ref"`
		Selector string `json:"selector"`
		Text     string `json:"text"`
		Submit   bool   `json:"submit"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := sess.Do(func(ctx context.Context) error {
		if err := browser.Type(ctx, browser.Target{Ref: in.Ref, Selector: in.Selector}, in.Text); err != nil {
			return err
		}
		if in.Submit {
			return browser.Press(ctx, "\r")
		}
		return nil
	}); err != nil {
		writeError(w, http.StatusBadGateway, "type_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePress(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		Key string `json:"key"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.Press(ctx, in.Key) }); err != nil {
		writeError(w, http.StatusBadGateway, "press_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleWait(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		Selector       string `json:"selector"`
		HiddenSelector string `json:"hidden_selector"`
		Text           string `json:"text"`
		URLContains    string `json:"url_contains"`
		CookieName     string `json:"cookie_name"`
		TimeoutMS      int    `json:"timeout_ms"`
	}
	if !decode(w, r, &in) {
		return
	}
	opts := browser.WaitOptions{
		Selector: in.Selector, HiddenSelector: in.HiddenSelector, Text: in.Text,
		URLContains: in.URLContains, CookieName: in.CookieName,
		Timeout: time.Duration(in.TimeoutMS) * time.Millisecond,
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.WaitForOptions(ctx, opts) }); err != nil {
		writeError(w, http.StatusGatewayTimeout, "wait_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleEval(w http.ResponseWriter, r *http.Request) {
	if !s.AllowEval {
		writeError(w, http.StatusForbidden, "eval_disabled", "eval is disabled for this user")
		return
	}
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		Expression string `json:"expression"`
	}
	if !decode(w, r, &in) {
		return
	}
	var result any
	if err := sess.Do(func(ctx context.Context) error {
		v, e := browser.Eval(ctx, in.Expression)
		result = v
		return e
	}); err != nil {
		writeError(w, http.StatusBadGateway, "eval_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	fullPage := r.URL.Query().Get("full_page") == "1" || r.URL.Query().Get("full_page") == "true"
	var buf []byte
	if err := sess.Do(func(ctx context.Context) error {
		b, e := browser.CapturePNG(ctx, fullPage)
		buf = b
		return e
	}); err != nil {
		writeError(w, http.StatusBadGateway, "screenshot_failed", err.Error())
		return
	}
	if r.URL.Query().Get("json") == "1" {
		out := map[string]any{"bytes": len(buf), "base64": base64.StdEncoding.EncodeToString(buf)}
		if name := r.URL.Query().Get("name"); name != "" {
			path := filepath.Join(s.User.ShotRoot, filepath.Base(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				if err := os.WriteFile(path, buf, 0o644); err == nil {
					out["path"] = path
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
}

func (s *Server) handleCookies(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	urls := r.URL.Query()["url"]
	var cookies any
	if err := sess.Do(func(ctx context.Context) error {
		c, e := browser.GetCookies(ctx, urls)
		cookies = c
		return e
	}); err != nil {
		writeError(w, http.StatusBadGateway, "cookies_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cookies": cookies})
}

func (s *Server) handleChallenges(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var out []browser.ChallengeCandidate
	if err := sess.Do(func(ctx context.Context) error {
		v, e := browser.DetectChallenges(ctx)
		out = v
		return e
	}); err != nil {
		writeError(w, http.StatusBadGateway, "detect_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"challenges": out})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var buf []byte
	if err := sess.Do(func(ctx context.Context) error {
		b, _, e := browser.QRCodePNG(ctx)
		buf = b
		return e
	}); err != nil {
		writeError(w, http.StatusNotFound, "qr_not_found", err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
}

func (s *Server) handleClickXY(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.ClickXY(ctx, in.X, in.Y) }); err != nil {
		writeError(w, http.StatusBadGateway, "click_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleScroll(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		DX int `json:"dx"`
		DY int `json:"dy"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.Scroll(ctx, in.DX, in.DY) }); err != nil {
		writeError(w, http.StatusBadGateway, "scroll_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- scripts ----

func (s *Server) handleScriptList(w http.ResponseWriter, _ *http.Request) {
	list, err := scripts.List(s.User.ScriptRoot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "script_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scripts": list})
}

func (s *Server) handleScriptRun(w http.ResponseWriter, r *http.Request) {
	if !s.AllowScripts {
		writeError(w, http.StatusForbidden, "scripts_disabled", "scripts are disabled for this user")
		return
	}
	name := r.PathValue("name")
	var in struct {
		Input     map[string]any `json:"input"`
		SessionID string         `json:"session_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	script, src, err := scripts.Load(s.User.ScriptRoot, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "script_not_found", err.Error())
		return
	}
	input := in.Input
	if input == nil {
		input = map[string]any{}
	}
	if in.SessionID != "" {
		input["session_id"] = in.SessionID
	}
	runner := jsruntime.New(s.User, s.Manager, os.Stderr)
	runner.AllowEval = s.AllowEval
	runner.AutoClose = true
	runner.Timeout = script.TimeoutDuration()
	result, err := runner.Run(src, input)
	if err != nil {
		writeError(w, http.StatusBadGateway, "script_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// ---- desktop ----

func (s *Server) handleDesktop(w http.ResponseWriter, _ *http.Request) {
	if s.Desktop == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "desktop": s.Desktop.Info()})
}

// ---- helpers ----

func (s *Server) session(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	sess, err := s.Manager.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", err.Error())
		return nil, false
	}
	return sess, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// TokenPath returns the per-user API token file.
func TokenPath(u users.User) string {
	return filepath.Join(u.Home, "data", "users", u.ID, "api_token")
}

// LoadOrCreateToken returns the persisted token, creating it on first use.
func LoadOrCreateToken(u users.User) (string, error) {
	path := TokenPath(u)
	if b, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(b)); token != "" {
			return token, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := "ac_" + hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}
