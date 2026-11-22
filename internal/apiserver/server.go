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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"

	"autochrome/internal/atomicfile"
	"autochrome/internal/attachments"
	"autochrome/internal/browser"
	"autochrome/internal/chrome"
	"autochrome/internal/cleaner"
	"autochrome/internal/desktop"
	"autochrome/internal/envdir"
	"autochrome/internal/filelock"
	"autochrome/internal/fingerprint"
	"autochrome/internal/jobs"
	"autochrome/internal/jsruntime"
	"autochrome/internal/maintenance"
	"autochrome/internal/mcpserver"
	"autochrome/internal/proxy"
	"autochrome/internal/scheduler"
	"autochrome/internal/scripts"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

// Version is reported by /api/v1/health.
const Version = "0.3.0"

// Server is the HTTP API bound to one user.
type Server struct {
	User         users.User
	ChromePath   string
	Chrome       *chrome.Manager
	Addr         string
	Token        string
	tokenMu      sync.RWMutex
	userMu       sync.RWMutex
	AllowOrigin  string
	AllowEval    bool
	AllowScripts bool
	Desktop      *desktop.Desktop
	Manager      *session.Manager
	Proxies      *proxy.Store
	Registry     *envdir.Registry
	Attachments  *attachments.Store
	Schedules    *scheduler.Store
	Jobs         *jobs.Manager

	EnableMCP  bool
	MCP        *mcpserver.Server
	MCPHandler http.Handler
	cleanMu    sync.Mutex

	interMu      sync.Mutex
	interactions map[string]*interaction

	vncMu     sync.Mutex
	vncTokens map[string]*vncToken

	events *eventHub
}

type interaction struct {
	SessionID string
	Expires   time.Time
}

// New returns a server for u.
func New(u users.User, chromePath string) *Server {
	return &Server{
		User:         u,
		ChromePath:   chromePath,
		Chrome:       chrome.NewManager(),
		Addr:         "127.0.0.1:8787",
		AllowEval:    u.AllowEval,
		AllowScripts: u.AllowScripts,
		Proxies:      proxy.NewStore(filepath.Join(u.Home, "data", "users", u.ID, "proxies.json")),
		Registry:     envdir.NewRegistry(u.RegistryPath()),
		Attachments:  attachments.NewStore(u.AttachmentRoot, u.AttachmentIndexPath()),
		Schedules:    scheduler.NewStore(filepath.Join(u.Home, "data", "users", u.ID, "schedules.json")),
		interactions: map[string]*interaction{},
		vncTokens:    map[string]*vncToken{},
		events:       newEventHub(),
	}
}

func (s *Server) userSnapshot() users.User {
	s.userMu.RLock()
	defer s.userMu.RUnlock()
	return s.User
}

// Run starts the HTTP server and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	s.Manager = session.NewManager(s.userSnapshot(), s.ChromePath)
	if s.Desktop != nil {
		s.Manager.SetDisplay(s.Desktop.Display())
	}
	defer s.Manager.CloseAll()
	s.Jobs = jobs.New(s.userSnapshot(), s.Manager, s.AllowEval, s.publish)
	defer s.Jobs.Close()
	if s.EnableMCP {
		mcpSrv := mcpserver.New(s.userSnapshot(), s.ChromePath)
		mcpSrv.Manager = s.Manager
		mcpSrv.Desktop = s.Desktop
		mcpSrv.Jobs = s.Jobs
		mcpSrv.AllowEval = s.AllowEval
		mcpSrv.AllowScripts = s.AllowScripts
		mcpSrv.Build()
		s.MCP = mcpSrv
		s.MCPHandler = mcpSrv.Handler()
	}
	go s.monitorVNCTokens(ctx)
	go s.runScheduler(ctx)

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
	mux.HandleFunc("GET /api/v1/openapi.yaml", s.handleOpenAPI)
	mux.HandleFunc("GET /api/v1/user", s.handleUser)
	mux.HandleFunc("POST /api/v1/token/rotate", s.handleTokenRotate)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/storage", s.handleStorageGet)
	mux.HandleFunc("PUT /api/v1/storage", s.handleStoragePut)
	mux.HandleFunc("PUT /api/v1/user/chrome", s.handleUserChrome)
	mux.HandleFunc("DELETE /api/v1/user/chrome", s.handleUserChromeUnbind)
	mux.HandleFunc("GET /api/v1/chrome", s.handleChrome)
	mux.HandleFunc("GET /api/v1/chrome/versions", s.handleChromeVersions)
	mux.HandleFunc("POST /api/v1/chrome/update", s.handleChromeUpdate)
	mux.HandleFunc("POST /api/v1/chrome/install", s.handleChromeInstall)
	mux.HandleFunc("PUT /api/v1/chrome/use", s.handleChromeUse)
	mux.HandleFunc("DELETE /api/v1/chrome/versions/{version}", s.handleChromeDelete)

	mux.HandleFunc("GET /api/v1/envs", s.handleEnvList)
	mux.HandleFunc("POST /api/v1/envs", s.handleEnvCreate)
	mux.HandleFunc("GET /api/v1/envs/{id}", s.handleEnvGet)
	mux.HandleFunc("PATCH /api/v1/envs/{id}", s.handleEnvUpdate)
	mux.HandleFunc("PUT /api/v1/envs/{id}", s.handleEnvUpdate)
	mux.HandleFunc("DELETE /api/v1/envs/{id}", s.handleEnvDelete)
	mux.HandleFunc("PUT /api/v1/envs/{id}/proxy", s.handleEnvBindProxy)
	mux.HandleFunc("DELETE /api/v1/envs/{id}/proxy", s.handleEnvUnbindProxy)
	mux.HandleFunc("PUT /api/v1/envs/{id}/size", s.handleEnvSize)
	mux.HandleFunc("POST /api/v1/envs/{id}/restart", s.handleEnvRestart)
	mux.HandleFunc("POST /api/v1/envs/{id}/clean", s.handleEnvClean)
	mux.HandleFunc("PUT /api/v1/envs/{id}/chrome", s.handleEnvChrome)
	mux.HandleFunc("DELETE /api/v1/envs/{id}/chrome", s.handleEnvChromeUnbind)
	mux.HandleFunc("GET /api/v1/envs/{id}/export", s.handleEnvExport)
	mux.HandleFunc("POST /api/v1/envs/import", s.handleEnvImport)

	mux.HandleFunc("GET /api/v1/containers", s.handleEnvList)
	mux.HandleFunc("POST /api/v1/containers", s.handleEnvCreate)
	mux.HandleFunc("POST /api/v1/containers/import", s.handleEnvImport)
	mux.HandleFunc("GET /api/v1/containers/{name}", s.containerAlias(s.handleEnvGet))
	mux.HandleFunc("PATCH /api/v1/containers/{name}", s.containerAlias(s.handleEnvUpdate))
	mux.HandleFunc("PUT /api/v1/containers/{name}", s.containerAlias(s.handleEnvUpdate))
	mux.HandleFunc("DELETE /api/v1/containers/{name}", s.containerAlias(s.handleEnvDelete))
	mux.HandleFunc("GET /api/v1/containers/{name}/export", s.containerAlias(s.handleEnvExport))
	mux.HandleFunc("PUT /api/v1/containers/{name}/proxy", s.containerAlias(s.handleEnvBindProxy))
	mux.HandleFunc("DELETE /api/v1/containers/{name}/proxy", s.containerAlias(s.handleEnvUnbindProxy))
	mux.HandleFunc("PUT /api/v1/containers/{name}/size", s.containerAlias(s.handleEnvSize))
	mux.HandleFunc("POST /api/v1/containers/{name}/restart", s.containerAlias(s.handleEnvRestart))
	mux.HandleFunc("POST /api/v1/containers/{name}/clean", s.containerAlias(s.handleEnvClean))
	mux.HandleFunc("PUT /api/v1/containers/{name}/chrome", s.containerAlias(s.handleEnvChrome))
	mux.HandleFunc("DELETE /api/v1/containers/{name}/chrome", s.containerAlias(s.handleEnvChromeUnbind))

	mux.HandleFunc("GET /api/v1/proxies", s.handleProxyList)
	mux.HandleFunc("POST /api/v1/proxies", s.handleProxyAdd)
	mux.HandleFunc("GET /api/v1/proxies/{id}", s.handleProxyGet)
	mux.HandleFunc("PATCH /api/v1/proxies/{id}", s.handleProxyUpdate)
	mux.HandleFunc("PUT /api/v1/proxies/{id}", s.handleProxyUpdate)
	mux.HandleFunc("DELETE /api/v1/proxies/{id}", s.handleProxyDelete)
	mux.HandleFunc("POST /api/v1/proxies/{id}/check", s.handleProxyCheck)

	mux.HandleFunc("GET /api/v1/sessions", s.handleSessionList)
	mux.HandleFunc("POST /api/v1/sessions", s.handleSessionCreate)
	mux.HandleFunc("POST /api/v1/sessions/batch", s.handleSessionBatch)
	mux.HandleFunc("DELETE /api/v1/sessions", s.handleSessionCloseAll)
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.handleSessionGet)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleSessionDelete)

	mux.HandleFunc("POST /api/v1/sessions/{id}/restart", s.handleSessionRestart)
	mux.HandleFunc("POST /api/v1/sessions/{id}/navigate", s.handleNavigate)
	mux.HandleFunc("POST /api/v1/sessions/{id}/reload", s.handleReload)
	mux.HandleFunc("POST /api/v1/sessions/{id}/back", s.handleBack)
	mux.HandleFunc("GET /api/v1/sessions/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /api/v1/sessions/{id}/click", s.handleClick)
	mux.HandleFunc("POST /api/v1/sessions/{id}/type", s.handleType)
	mux.HandleFunc("POST /api/v1/sessions/{id}/press", s.handlePress)
	mux.HandleFunc("POST /api/v1/sessions/{id}/wait", s.handleWait)
	mux.HandleFunc("POST /api/v1/sessions/{id}/eval", s.handleEval)
	mux.HandleFunc("GET /api/v1/sessions/{id}/screenshot", s.handleScreenshot)
	mux.HandleFunc("GET /api/v1/sessions/{id}/cookies", s.handleCookies)
	mux.HandleFunc("POST /api/v1/sessions/{id}/cookies", s.handleCookiesSet)
	mux.HandleFunc("GET /api/v1/sessions/{id}/challenges", s.handleChallenges)
	mux.HandleFunc("GET /api/v1/sessions/{id}/qr", s.handleQR)
	mux.HandleFunc("POST /api/v1/sessions/{id}/click_xy", s.handleClickXY)
	mux.HandleFunc("POST /api/v1/sessions/{id}/scroll", s.handleScroll)
	mux.HandleFunc("GET /api/v1/sessions/{id}/auth", s.handleAuthGet)
	mux.HandleFunc("POST /api/v1/sessions/{id}/auth", s.handleAuthSet)
	mux.HandleFunc("GET /api/v1/sessions/{id}/auth/export", s.handleAuthExport)
	mux.HandleFunc("POST /api/v1/sessions/{id}/auth/import", s.handleAuthImport)
	mux.HandleFunc("GET /api/v1/sessions/{id}/interaction", s.handleInteraction)

	mux.HandleFunc("GET /api/v1/schedules", s.handleScheduleList)
	mux.HandleFunc("POST /api/v1/schedules", s.handleScheduleAdd)
	mux.HandleFunc("GET /api/v1/schedules/{id}", s.handleScheduleGet)
	mux.HandleFunc("PATCH /api/v1/schedules/{id}", s.handleScheduleUpdate)
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.handleScheduleDelete)
	mux.HandleFunc("POST /api/v1/schedules/{id}/run", s.handleScheduleRun)

	mux.HandleFunc("GET /api/v1/scripts", s.handleScriptList)
	mux.HandleFunc("GET /api/v1/scripts/{name}", s.handleScriptGet)
	mux.HandleFunc("PUT /api/v1/scripts/{name}", s.handleScriptWrite)
	mux.HandleFunc("DELETE /api/v1/scripts/{name}", s.handleScriptDelete)
	mux.HandleFunc("POST /api/v1/scripts/{name}/run", s.handleScriptRun)
	mux.HandleFunc("POST /api/v1/scripts/{name}/jobs", s.handleScriptJobStart)

	mux.HandleFunc("GET /api/v1/jobs", s.handleJobList)
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.handleJobGet)
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs", s.handleJobLogs)
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.handleJobCancel)

	mux.HandleFunc("GET /api/v1/attachments", s.handleAttachmentList)
	mux.HandleFunc("POST /api/v1/attachments", s.handleAttachmentAdd)
	mux.HandleFunc("GET /api/v1/attachments/{id}", s.handleAttachmentGet)
	mux.HandleFunc("PATCH /api/v1/attachments/{id}", s.handleAttachmentRename)
	mux.HandleFunc("GET /api/v1/attachments/{id}/download", s.handleAttachmentDownload)
	mux.HandleFunc("DELETE /api/v1/attachments/{id}", s.handleAttachmentDelete)

	mux.HandleFunc("GET /api/v1/envs/{id}/attachments", s.handleContainerAttachmentList)
	mux.HandleFunc("POST /api/v1/envs/{id}/attachments", s.handleContainerAttachmentAdd)
	mux.HandleFunc("GET /api/v1/envs/{id}/attachments/{aid}", s.handleContainerAttachmentGet)
	mux.HandleFunc("PATCH /api/v1/envs/{id}/attachments/{aid}", s.handleContainerAttachmentRename)
	mux.HandleFunc("GET /api/v1/envs/{id}/attachments/{aid}/download", s.handleContainerAttachmentDownload)
	mux.HandleFunc("DELETE /api/v1/envs/{id}/attachments/{aid}", s.handleContainerAttachmentDelete)

	mux.HandleFunc("GET /api/v1/containers/{name}/attachments", s.handleContainerAttachmentList)
	mux.HandleFunc("POST /api/v1/containers/{name}/attachments", s.handleContainerAttachmentAdd)
	mux.HandleFunc("GET /api/v1/containers/{name}/attachments/{aid}", s.handleContainerAttachmentGet)
	mux.HandleFunc("PATCH /api/v1/containers/{name}/attachments/{aid}", s.handleContainerAttachmentRename)
	mux.HandleFunc("GET /api/v1/containers/{name}/attachments/{aid}/download", s.handleContainerAttachmentDownload)
	mux.HandleFunc("DELETE /api/v1/containers/{name}/attachments/{aid}", s.handleContainerAttachmentDelete)

	mux.HandleFunc("GET /api/v1/desktop", s.handleDesktop)
	mux.HandleFunc("POST /api/v1/desktop/token", s.handleDesktopToken)

	mux.HandleFunc("GET /i/{token}", s.handleInteractionPage)
	mux.HandleFunc("GET /i/{token}/state", s.handleInteractionState)
	mux.HandleFunc("GET /i/{token}/screenshot", s.handleInteractionScreenshot)
	mux.HandleFunc("GET /i/{token}/qr", s.handleInteractionQR)
	mux.HandleFunc("GET /i/{token}/vnc", s.handleInteractionVNC)
	mux.HandleFunc("POST /i/{token}/code", s.handleInteractionCode)
	mux.HandleFunc("POST /i/{token}/click_xy", s.handleInteractionClickXY)

	mux.HandleFunc("GET /vnc", s.handleBrowserAccess)
	mux.HandleFunc("GET /{$}", s.handleBrowserAccess)
	mux.HandleFunc("GET /vnc/{token}", s.handleVNC)
	mux.HandleFunc("GET /vnc/{token}/{path...}", s.handleVNC)

	if s.MCPHandler != nil {
		mux.Handle("/mcp", s.MCPHandler)
		mux.Handle("/mcp/", s.MCPHandler)
	}
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/vnc" || r.URL.Path == "/api/v1/health" || r.URL.Path == "/api/v1/openapi.yaml" || strings.HasPrefix(r.URL.Path, "/i/") || strings.HasPrefix(r.URL.Path, "/vnc/") {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		s.tokenMu.RLock()
		current := s.Token
		s.tokenMu.RUnlock()
		if subtle.ConstantTimeCompare([]byte(token), []byte(current)) != 1 {
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

func (s *Server) handleTokenRotate(w http.ResponseWriter, _ *http.Request) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeError(w, http.StatusInternalServerError, "token_rotate_failed", err.Error())
		return
	}
	token := "ac_" + hex.EncodeToString(buf)
	if err := SaveToken(s.userSnapshot(), token); err != nil {
		writeError(w, http.StatusInternalServerError, "token_rotate_failed", err.Error())
		return
	}
	s.tokenMu.Lock()
	s.Token = token
	s.tokenMu.Unlock()
	s.publish("token.rotated", map[string]any{})
	writeJSON(w, http.StatusOK, map[string]any{"token": token})
}

func (s *Server) handleUser(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.userSnapshot())
}

// ---- environments ----

func (s *Server) handleEnvList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"envs": s.listEnvs()})
}

func (s *Server) listEnvs() []envdir.Meta {
	u := s.userSnapshot()
	byID := map[string]envdir.Meta{}
	if list, err := s.Registry.List(); err == nil {
		for _, m := range list {
			dir := m.Dir
			if dir == "" {
				dir = filepath.Join(u.EnvRoot, envdir.Name(m.Platform, m.ID))
			}
			if _, err := os.Stat(dir); err != nil {
				_ = s.Registry.Delete(m.ID)
				continue
			}
			byID[m.ID] = m
		}
	}
	if list, err := envdir.List(u.EnvRoot); err == nil {
		for _, m := range list {
			if _, ok := byID[m.ID]; ok {
				continue
			}
			byID[m.ID] = m
			_ = s.Registry.Put(m)
		}
	}
	out := make([]envdir.Meta, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *Server) findEnvByName(name string) (envdir.Meta, string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return envdir.Meta{}, "", false
	}
	u := s.userSnapshot()
	for _, m := range s.listEnvs() {
		if m.Name == name || m.ID == name {
			dir := m.Dir
			if dir == "" {
				dir = filepath.Join(u.EnvRoot, envdir.Name(m.Platform, m.ID))
			}
			return m, dir, true
		}
	}
	return envdir.Meta{}, "", false
}

func (s *Server) findEnv(id string) (envdir.Meta, string, bool) {
	if m, ok := s.Registry.Get(id); ok {
		dir := m.Dir
		if dir == "" {
			dir = filepath.Join(s.userSnapshot().EnvRoot, envdir.Name(m.Platform, m.ID))
		}
		if _, err := os.Stat(dir); err == nil {
			return m, dir, true
		}
		_ = s.Registry.Delete(id)
	}
	if m, dir, ok := envdir.Find(s.userSnapshot().EnvRoot, id); ok {
		_ = s.Registry.Put(m)
		return m, dir, true
	}
	return s.findEnvByName(id)
}

func (s *Server) handleEnvCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		Description   string   `json:"description"`
		Tags          []string `json:"tags"`
		Platform      string   `json:"platform"`
		ChromeVersion string   `json:"chrome_version"`
	}
	if !decode(w, r, &in) {
		return
	}
	if err := s.validateChrome(in.ChromeVersion); err != nil {
		writeError(w, http.StatusBadRequest, "chrome_version_invalid", err.Error())
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "id is required")
		return
	}
	if in.Platform == "" {
		in.Platform = "default"
	}
	if _, _, ok := s.findEnv(in.ID); ok {
		writeError(w, http.StatusConflict, "env_exists", fmt.Sprintf("container %q already exists", in.ID))
		return
	}
	if in.Name == "" {
		in.Name = in.ID
	}
	if _, _, ok := s.findEnvByName(in.Name); ok {
		writeError(w, http.StatusConflict, "env_name_exists", fmt.Sprintf("container name %q already exists", in.Name))
		return
	}
	fp := fingerprint.Generate(in.ID)
	dir, err := envdir.Create(s.userSnapshot().EnvRoot, in.Platform, in.ID, fp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_create_failed", err.Error())
		return
	}
	meta, _ := envdir.ReadMeta(dir)
	meta.Name = in.Name
	meta.Description = in.Description
	meta.Tags = in.Tags
	if in.ChromeVersion != "" {
		meta.ChromeVersion = in.ChromeVersion
	}
	meta.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := envdir.WriteMeta(dir, meta); err != nil {
		writeError(w, http.StatusInternalServerError, "env_create_failed", err.Error())
		return
	}
	_ = s.Registry.Put(meta)
	writeJSON(w, http.StatusCreated, map[string]any{"dir": dir, "fingerprint": fp, "env": meta})
}

// ---- sessions ----

func (s *Server) handleSessionList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"sessions": s.Manager.List()})
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Env           string `json:"env"`
		Platform      string `json:"platform"`
		URL           string `json:"url"`
		Proxy         string `json:"proxy"`
		ProxyID       string `json:"proxy_id"`
		ChromeVersion string `json:"chrome_version"`
		Headless      bool   `json:"headless"`
	}
	if !decode(w, r, &in) {
		return
	}
	proxyURL := in.Proxy
	if in.ProxyID != "" {
		p, err := s.Proxies.Get(in.ProxyID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "proxy_not_found", err.Error())
			return
		}
		if !p.Enabled {
			writeError(w, http.StatusBadRequest, "proxy_disabled", "proxy is disabled")
			return
		}
		proxyURL = p.URL
	}
	sess, err := s.Manager.Open(in.Env, in.Platform, in.URL, proxyURL, in.Headless, in.ChromeVersion)
	if err != nil {
		writeError(w, http.StatusBadRequest, "open_failed", err.Error())
		return
	}
	info := sess.Info()
	out := map[string]any{"session_id": sess.ID, "env": sess.EnvID, "proxy_id": in.ProxyID, "chrome_version": info.ChromeVersion}
	_ = sess.Do(func(ctx context.Context) error {
		out["url"], _ = browser.Location(ctx)
		out["title"], _ = browser.Title(ctx)
		return nil
	})
	s.publish("session.opened", out)
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) handleSessionBatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Envs          []string `json:"envs"`
		URL           string   `json:"url"`
		ProxyID       string   `json:"proxy_id"`
		ChromeVersion string   `json:"chrome_version"`
		Headless      bool     `json:"headless"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Envs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "envs is required")
		return
	}
	proxyURL := ""
	if in.ProxyID != "" {
		p, err := s.Proxies.Get(in.ProxyID)
		if err != nil || !p.Enabled {
			writeError(w, http.StatusBadRequest, "proxy_unavailable", "proxy not found or disabled")
			return
		}
		proxyURL = p.URL
	}
	results := make([]map[string]any, 0, len(in.Envs))
	for _, env := range in.Envs {
		result := map[string]any{"env": env}
		sess, err := s.Manager.Open(env, "", in.URL, proxyURL, in.Headless, in.ChromeVersion)
		if err != nil {
			result["error"] = err.Error()
			results = append(results, result)
			continue
		}
		result["session_id"] = sess.ID
		result["chrome_version"] = sess.Info().ChromeVersion
		_ = sess.Do(func(ctx context.Context) error {
			result["url"], _ = browser.Location(ctx)
			result["title"], _ = browser.Title(ctx)
			return nil
		})
		results = append(results, result)
		s.publish("session.opened", result)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sess.Info())
}

func (s *Server) handleSessionCloseAll(w http.ResponseWriter, _ *http.Request) {
	s.Manager.CloseAll()
	s.publish("sessions.closed", map[string]any{"all": true})
	writeJSON(w, http.StatusOK, map[string]any{"closed": true})
}

func (s *Server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Manager.Close(id); err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", err.Error())
		return
	}
	s.publish("session.closed", map[string]any{"session_id": id})
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

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.Reload(ctx) }); err != nil {
		writeError(w, http.StatusBadGateway, "reload_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleBack(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.Back(ctx) }); err != nil {
		writeError(w, http.StatusBadGateway, "back_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
			dir := sess.ScreenshotDir
			if dir == "" {
				dir = filepath.Join(sess.ProfileDir, "screenshots")
			}
			path := filepath.Join(dir, filepath.Base(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				if err := os.WriteFile(path, buf, 0o600); err == nil {
					out["path"] = path
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
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

func (s *Server) handleCookiesSet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var in struct {
		Cookies []*network.CookieParam `json:"cookies"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Cookies) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "cookies is required")
		return
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.SetCookies(ctx, in.Cookies) }); err != nil {
		writeError(w, http.StatusBadGateway, "cookies_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(in.Cookies)})
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
	w.Header().Set("Cache-Control", "no-store")
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
	list, err := scripts.List(s.userSnapshot().ScriptRoot)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "script_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scripts": list})
}

func (s *Server) handleScriptGet(w http.ResponseWriter, r *http.Request) {
	script, src, err := scripts.Load(s.userSnapshot().ScriptRoot, r.PathValue("name"))
	if err != nil {
		writeError(w, http.StatusNotFound, "script_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": script.Name, "description": script.Description,
		"timeout": script.Timeout, "inputSchema": script.InputSchema,
		"source": src,
	})
}

func (s *Server) handleScriptWrite(w http.ResponseWriter, r *http.Request) {
	if !s.AllowScripts {
		writeError(w, http.StatusForbidden, "scripts_disabled", "scripts are disabled for this user")
		return
	}
	var in struct {
		Source      string         `json:"source"`
		Description string         `json:"description"`
		Timeout     string         `json:"timeout"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	if !decode(w, r, &in) {
		return
	}
	var manifest map[string]any
	if in.Description != "" || in.Timeout != "" || in.InputSchema != nil {
		manifest = map[string]any{}
		if in.Description != "" {
			manifest["description"] = in.Description
		}
		if in.Timeout != "" {
			manifest["timeout"] = in.Timeout
		}
		if in.InputSchema != nil {
			manifest["inputSchema"] = in.InputSchema
		}
	}
	if err := scripts.Write(s.userSnapshot().ScriptRoot, r.PathValue("name"), in.Source, manifest); err != nil {
		writeError(w, http.StatusBadRequest, "script_write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": r.PathValue("name")})
}

func (s *Server) handleScriptDelete(w http.ResponseWriter, r *http.Request) {
	if !s.AllowScripts {
		writeError(w, http.StatusForbidden, "scripts_disabled", "scripts are disabled for this user")
		return
	}
	if err := scripts.Delete(s.userSnapshot().ScriptRoot, r.PathValue("name")); err != nil {
		writeError(w, http.StatusNotFound, "script_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
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
	script, src, err := scripts.Load(s.userSnapshot().ScriptRoot, name)
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
	runner := jsruntime.New(s.userSnapshot(), s.Manager, os.Stderr)
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

// ---- proxy management ----

type proxyInput struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Enabled *bool    `json:"enabled"`
	Tags    []string `json:"tags"`
}

func (s *Server) handleProxyList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Proxies.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "proxy_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proxies": list})
}

func (s *Server) handleProxyAdd(w http.ResponseWriter, r *http.Request) {
	var in proxyInput
	if !decode(w, r, &in) {
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	p, err := s.Proxies.Add(proxy.Proxy{ID: in.ID, Name: in.Name, URL: in.URL, Enabled: enabled, Tags: in.Tags})
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy_add_failed", err.Error())
		return
	}
	s.publish("proxy.created", p)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) handleProxyGet(w http.ResponseWriter, r *http.Request) {
	p, err := s.Proxies.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "proxy_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    *string   `json:"name"`
		URL     *string   `json:"url"`
		Enabled *bool     `json:"enabled"`
		Tags    *[]string `json:"tags"`
	}
	if !decode(w, r, &in) {
		return
	}
	p, err := s.Proxies.Update(r.PathValue("id"), func(p *proxy.Proxy) error {
		if in.Name != nil {
			p.Name = *in.Name
		}
		if in.URL != nil {
			p.URL = *in.URL
		}
		if in.Enabled != nil {
			p.Enabled = *in.Enabled
		}
		if in.Tags != nil {
			p.Tags = *in.Tags
		}
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy_update_failed", err.Error())
		return
	}
	s.publish("proxy.updated", p)
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Proxies.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "proxy_not_found", err.Error())
		return
	}
	s.publish("proxy.deleted", map[string]any{"id": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleProxyCheck(w http.ResponseWriter, r *http.Request) {
	p, err := s.Proxies.Check(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "proxy_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ---- environment management ----

func (s *Server) handleEnvGet(w http.ResponseWriter, r *http.Request) {
	meta, _, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	writeJSON(w, http.StatusOK, meta)
}

func (s *Server) handleEnvUpdate(w http.ResponseWriter, r *http.Request) {
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	var in struct {
		Name        *string   `json:"name"`
		Description *string   `json:"description"`
		Tags        *[]string `json:"tags"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name != "" {
			if existing, _, ok := s.findEnvByName(name); ok && existing.ID != meta.ID {
				writeError(w, http.StatusConflict, "env_name_exists", fmt.Sprintf("container name %q already exists", name))
				return
			}
		}
		meta.Name = name
	}
	if in.Description != nil {
		meta.Description = *in.Description
	}
	if in.Tags != nil {
		meta.Tags = *in.Tags
	}
	meta.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := envdir.WriteMeta(dir, meta); err != nil {
		writeError(w, http.StatusInternalServerError, "env_update_failed", err.Error())
		return
	}
	_ = s.Registry.Put(meta)
	s.publish("env.updated", meta)
	writeJSON(w, http.StatusOK, meta)
}

func (s *Server) containerAlias(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if meta, _, ok := s.findEnvByName(name); ok {
			r.SetPathValue("id", meta.ID)
		} else {
			r.SetPathValue("id", name)
		}
		next(w, r)
	}
}

func (s *Server) handleEnvDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	meta, dir, ok := s.findEnv(id)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	if dir == s.userSnapshot().Home || dir == s.userSnapshot().EnvRoot || dir == s.userSnapshot().AttachmentRoot || dir == filepath.Join(s.userSnapshot().Home, "data") {
		writeError(w, http.StatusBadRequest, "unsafe_path", "refusing to delete a storage root")
		return
	}
	s.Manager.CloseEnv(id)
	if err := envdir.RemoveDir(dir); err != nil {
		writeError(w, http.StatusInternalServerError, "env_delete_failed", err.Error())
		return
	}
	_ = s.Registry.Delete(meta.ID)
	s.publish("env.deleted", meta)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) handleEnvBindProxy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProxyID string `json:"proxy_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	if in.ProxyID == "" {
		updated, err := envdir.SetProxyDir(dir, meta, "", "")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "env_bind_failed", err.Error())
			return
		}
		_ = s.Registry.Put(updated)
		writeJSON(w, http.StatusOK, updated)
		return
	}
	p, err := s.Proxies.Get(in.ProxyID)
	if err != nil {
		writeError(w, http.StatusNotFound, "proxy_not_found", err.Error())
		return
	}
	if !p.Enabled {
		writeError(w, http.StatusBadRequest, "proxy_disabled", "proxy is disabled")
		return
	}
	updated, err := envdir.SetProxyDir(dir, meta, p.ID, p.URL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_bind_failed", err.Error())
		return
	}
	_ = s.Registry.Put(updated)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleEnvUnbindProxy(w http.ResponseWriter, r *http.Request) {
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	updated, err := envdir.SetProxyDir(dir, meta, "", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_unbind_failed", err.Error())
		return
	}
	_ = s.Registry.Put(updated)
	writeJSON(w, http.StatusOK, updated)
}

// ---- interaction page ----

func (s *Server) newInteraction(sessionID string) (string, time.Time) {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	token := hex.EncodeToString(buf)
	expires := time.Now().Add(15 * time.Minute)
	s.interMu.Lock()
	for k, v := range s.interactions {
		if time.Now().After(v.Expires) {
			delete(s.interactions, k)
		}
	}
	s.interactions[token] = &interaction{SessionID: sessionID, Expires: expires}
	s.interMu.Unlock()
	return token, expires
}

func (s *Server) lookupInteraction(token string) (string, bool) {
	s.interMu.Lock()
	defer s.interMu.Unlock()
	v, ok := s.interactions[token]
	if !ok {
		return "", false
	}
	if time.Now().After(v.Expires) {
		delete(s.interactions, token)
		return "", false
	}
	return v.SessionID, true
}

func (s *Server) interactionSession(w http.ResponseWriter, r *http.Request) (*session.Session, bool) {
	sid, ok := s.lookupInteraction(r.PathValue("token"))
	if !ok {
		writeError(w, http.StatusNotFound, "interaction_not_found", "interaction token expired or unknown")
		return nil, false
	}
	sess, err := s.Manager.Get(sid)
	if err != nil {
		writeError(w, http.StatusNotFound, "session_not_found", err.Error())
		return nil, false
	}
	return sess, true
}

func (s *Server) handleInteraction(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	token, expires := s.newInteraction(sess.ID)
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		host = s.Addr
	}
	base := scheme + "://" + host + "/i/" + token
	vncURL := ""
	if s.Desktop != nil {
		vt := s.newVNCToken(s.Desktop.Info().WebPort)
		vncURL = scheme + "://" + host + "/vnc/" + vt + "/vnc.html?autoconnect=1&resize=scale&reconnect=1&path=vnc/" + vt + "/websockify"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":     sess.ID,
		"page_url":       base,
		"screenshot_url": base + "/screenshot",
		"qr_url":         base + "/qr",
		"state_url":      base + "/state",
		"vnc_url":        vncURL,
		"expires_at":     expires.Format(time.RFC3339),
	})
}

const interactionHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>AutoChrome interaction</title>
<style>
body{font-family:system-ui,sans-serif;margin:20px;background:#111;color:#eee}
a{color:#6cf} img{border:1px solid #333;max-width:100%}
.row{display:flex;gap:20px;align-items:flex-start;flex-wrap:wrap}
input,button{padding:8px;margin:4px 0;font-size:14px}
pre{background:#000;padding:10px;overflow:auto;max-height:120px}
</style></head><body>
<h1>AutoChrome interaction</h1>
<p id="meta">Loading...</p>
<div class="row">
<div><h2>Screen</h2><img id="shot" src="__BASE__/screenshot" alt="screen"></div>
<div><h2>QR / challenge</h2><img id="qr" src="__BASE__/qr" alt="qr" onerror="this.style.display='none'"><p><a href="__BASE__/vnc" target="_blank">Open VNC</a></p></div>
</div>
<form id="codeForm">
<input id="code" placeholder="SMS / email code" autocomplete="one-time-code">
<input id="selector" placeholder="optional CSS selector">
<label><input type="checkbox" id="submit" checked> press Enter</label>
<button type="submit">Submit code</button>
</form>
<pre id="result"></pre>
<script>
const base = "__BASE__";
async function refresh(){
  try {
    const s = await fetch(base + "/state").then(r => r.json());
    document.getElementById("meta").textContent = (s.title || "") + "  " + (s.url || "");
  } catch(e) {}
  document.getElementById("shot").src = base + "/screenshot?t=" + Date.now();
  document.getElementById("qr").src = base + "/qr?t=" + Date.now();
}
document.getElementById("qr").addEventListener("load", function(){ this.style.display = "block"; });
document.getElementById("codeForm").addEventListener("submit", async function(e){
  e.preventDefault();
  const body = {code: document.getElementById("code").value, selector: document.getElementById("selector").value, submit: document.getElementById("submit").checked};
  const r = await fetch(base + "/code", {method:"POST", headers:{"Content-Type":"application/json"}, body: JSON.stringify(body)});
  document.getElementById("result").textContent = await r.text();
});
setInterval(refresh, 2500);
refresh();
</script>
</body></html>`

func (s *Server) handleInteractionPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.lookupInteraction(r.PathValue("token")); !ok {
		writeError(w, http.StatusNotFound, "interaction_not_found", "interaction token expired or unknown")
		return
	}
	base := "/i/" + r.PathValue("token")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(strings.ReplaceAll(interactionHTML, "__BASE__", base)))
}

func (s *Server) handleInteractionState(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.interactionSession(w, r)
	if !ok {
		return
	}
	var out struct {
		SessionID  string                       `json:"session_id"`
		URL        string                       `json:"url"`
		Title      string                       `json:"title"`
		Challenges []browser.ChallengeCandidate `json:"challenges"`
		VNCEnabled bool                         `json:"vnc_enabled"`
	}
	out.SessionID = sess.ID
	out.VNCEnabled = s.Desktop != nil
	if err := sess.Do(func(ctx context.Context) error {
		out.URL, _ = browser.Location(ctx)
		out.Title, _ = browser.Title(ctx)
		out.Challenges, _ = browser.DetectChallenges(ctx)
		return nil
	}); err != nil {
		writeError(w, http.StatusBadGateway, "state_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleInteractionScreenshot(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.interactionSession(w, r)
	if !ok {
		return
	}
	fullPage := r.URL.Query().Get("full_page") == "1"
	var buf []byte
	if err := sess.Do(func(ctx context.Context) error {
		b, e := browser.CapturePNG(ctx, fullPage)
		buf = b
		return e
	}); err != nil {
		writeError(w, http.StatusBadGateway, "screenshot_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
}

func (s *Server) handleInteractionQR(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.interactionSession(w, r)
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
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	_, _ = w.Write(buf)
}

func (s *Server) handleInteractionVNC(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.lookupInteraction(r.PathValue("token")); !ok {
		writeError(w, http.StatusNotFound, "interaction_not_found", "interaction token expired or unknown")
		return
	}
	if s.Desktop == nil {
		writeError(w, http.StatusNotFound, "vnc_disabled", "VNC is not enabled")
		return
	}
	token := s.newVNCToken(s.Desktop.Info().WebPort)
	http.Redirect(w, r, "/vnc/"+token+"/vnc.html?autoconnect=1&resize=scale&reconnect=1&path=vnc/"+token+"/websockify", http.StatusFound)
}

func (s *Server) handleInteractionCode(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.interactionSession(w, r)
	if !ok {
		return
	}
	var in struct {
		Code     string `json:"code"`
		Selector string `json:"selector"`
		Submit   bool   `json:"submit"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Code == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}
	selector := in.Selector
	if selector == "" {
		var challenges []browser.ChallengeCandidate
		_ = sess.Do(func(ctx context.Context) error {
			challenges, _ = browser.DetectChallenges(ctx)
			return nil
		})
		for _, c := range challenges {
			if c.Type == "otp" {
				selector = c.Selector
				break
			}
		}
	}
	if selector == "" {
		writeError(w, http.StatusNotFound, "otp_not_found", "no OTP input detected; pass selector")
		return
	}
	if err := sess.Do(func(ctx context.Context) error {
		if err := browser.Type(ctx, browser.Target{Selector: selector}, in.Code); err != nil {
			return err
		}
		if in.Submit {
			return browser.Press(ctx, "\r")
		}
		return nil
	}); err != nil {
		writeError(w, http.StatusBadGateway, "code_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleInteractionClickXY(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.interactionSession(w, r)
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

// ---- storage ----

func (s *Server) storageInfo() map[string]any {
	u := s.userSnapshot()
	return map[string]any{
		"env_root":        u.EnvRoot,
		"attachment_root": u.AttachmentRoot,
		"script_root":     u.ScriptRoot,
		"screenshot_root": u.ShotRoot,
		"data_root":       filepath.Join(u.Home, "data"),
	}
}

func (s *Server) handleStorageGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.storageInfo())
}

func (s *Server) handleStoragePut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EnvRoot        string `json:"env_root"`
		AttachmentRoot string `json:"attachment_root"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.userMu.Lock()
	err := s.User.SetStorage(in.EnvRoot, in.AttachmentRoot)
	u := s.User
	s.userMu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadRequest, "storage_update_failed", err.Error())
		return
	}
	s.Manager.SetUser(u)
	s.Attachments.SetRoot(u.AttachmentRoot)
	if s.Jobs != nil {
		s.Jobs.SetUser(u)
	}
	if s.MCP != nil {
		s.MCP.SetUser(u)
	}
	s.publish("storage.updated", s.storageInfo())
	writeJSON(w, http.StatusOK, s.storageInfo())
}

func (s *Server) handleSessionRestart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	sess, err := s.Manager.Restart(r.PathValue("id"), in.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "restart_failed", err.Error())
		return
	}
	out := map[string]any{"session_id": sess.ID, "env": sess.EnvID}
	info := sess.Info()
	out["url"], out["chrome_version"] = info.URL, info.ChromeVersion
	s.publish("session.restarted", out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEnvRestart(w http.ResponseWriter, r *http.Request) {
	meta, _, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	sess, err := s.Manager.RestartEnv(meta.ID, in.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, "restart_failed", err.Error())
		return
	}
	info := sess.Info()
	out := map[string]any{
		"session_id": sess.ID, "env": sess.EnvID, "url": info.URL,
		"chrome_version": info.ChromeVersion,
	}
	s.publish("session.restarted", out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleEnvSize(w http.ResponseWriter, r *http.Request) {
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	var in struct {
		Width   int   `json:"width"`
		Height  int   `json:"height"`
		Restart *bool `json:"restart"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Width < 800 || in.Width > 3840 || in.Height < 600 || in.Height > 2160 {
		writeError(w, http.StatusBadRequest, "invalid_size", "width must be 800-3840, height 600-2160")
		return
	}
	updated, err := envdir.SetSizeDir(dir, meta, in.Width, in.Height)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "env_size_failed", err.Error())
		return
	}
	_ = s.Registry.Put(updated)
	restart := true
	if in.Restart != nil {
		restart = *in.Restart
	}
	sessionID := ""
	if restart {
		if sess, ok := s.Manager.SessionForEnv(meta.ID); ok {
			restarted, err := s.Manager.Restart(sess.ID, "")
			if err != nil {
				writeError(w, http.StatusBadGateway, "session_restart_failed", err.Error())
				return
			}
			sessionID = restarted.ID
		}
	}
	s.publish("env.updated", updated)
	writeJSON(w, http.StatusOK, map[string]any{"env": updated, "session_id": sessionID})
}

func (s *Server) handleEnvClean(w http.ResponseWriter, r *http.Request) {
	meta, _, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	var in struct {
		Cache       *bool `json:"cache"`
		Screenshots *bool `json:"screenshots"`
		Attachments *bool `json:"attachments"`
		Downloads   *bool `json:"downloads"`
		Restart     *bool `json:"restart"`
		DryRun      bool  `json:"dry_run"`
	}
	if !decode(w, r, &in) {
		return
	}
	opts := cleaner.Options{Cache: true, Downloads: true, DryRun: in.DryRun}
	if in.Cache != nil {
		opts.Cache = *in.Cache
	}
	if in.Screenshots != nil {
		opts.Screenshots = *in.Screenshots
	}
	if in.Attachments != nil {
		opts.Attachments = *in.Attachments
	}
	if in.Downloads != nil {
		opts.Downloads = *in.Downloads
	}
	restart := true
	if in.Restart != nil {
		restart = *in.Restart
	}
	s.cleanMu.Lock()
	result, sess, err := maintenance.Clean(s.Manager, meta, opts, restart)
	s.cleanMu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadGateway, "clean_failed", err.Error())
		return
	}
	out := map[string]any{"clean": result, "restarted": sess != nil}
	if sess != nil {
		out["session_id"] = sess.ID
		out["url"] = sess.Info().URL
	}
	s.publish("container.cleaned", out)
	writeJSON(w, http.StatusOK, out)
}

// ---- environment export / import ----

func (s *Server) handleEnvExport(w http.ResponseWriter, r *http.Request) {
	meta, dir, ok := s.findEnv(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "environment not found")
		return
	}
	if r.URL.Query().Get("close") == "1" {
		s.Manager.CloseEnv(meta.ID)
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", envdir.Sanitize(meta.ID)+".tar.gz"))
	if err := envdir.Export(w, dir); err != nil {
		fmt.Fprintf(os.Stderr, "env export %s: %v\n", meta.ID, err)
	}
}

func (s *Server) handleEnvImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30)
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
	meta, dir, err := envdir.Import(s.userSnapshot().EnvRoot, file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "env_import_failed", err.Error())
		return
	}
	if existing, ok := s.Registry.Get(meta.ID); ok && existing.Dir != "" && existing.Dir != dir {
		if _, err := os.Stat(existing.Dir); err == nil {
			_ = envdir.RemoveDir(dir)
			writeError(w, http.StatusConflict, "env_exists", fmt.Sprintf("environment %q already exists at %s", meta.ID, existing.Dir))
			return
		}
		_ = s.Registry.Delete(meta.ID)
	}
	name := meta.Name
	if name == "" {
		name = meta.ID
	}
	if s.Registry.NameTaken(name, meta.ID) {
		_ = envdir.RemoveDir(dir)
		writeError(w, http.StatusConflict, "env_name_exists", fmt.Sprintf("container name %q already exists", name))
		return
	}
	_ = s.Registry.Put(meta)
	writeJSON(w, http.StatusCreated, map[string]any{"env": meta, "dir": dir})
}

// ---- attachments ----

const maxAttachmentSize = 512 << 20

func (s *Server) handleAttachmentList(w http.ResponseWriter, _ *http.Request) {
	list, err := s.Attachments.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "attachment_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": list, "root": s.Attachments.Root()})
}

func (s *Server) handleAttachmentAdd(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentSize+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "file field is required")
		return
	}
	defer file.Close()
	name := r.FormValue("name")
	if name == "" && hdr != nil {
		name = hdr.Filename
	}
	saved, err := s.Attachments.Save(name, file, maxAttachmentSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, "attachment_save_failed", err.Error())
		return
	}
	s.publish("attachment.created", saved)
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handleAttachmentGet(w http.ResponseWriter, r *http.Request) {
	f, err := s.Attachments.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleAttachmentRename(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	f, err := s.Attachments.Rename(r.PathValue("id"), in.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "attachment_rename_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	f, err := s.Attachments.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment_not_found", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Name))
	if f.MIME != "" {
		w.Header().Set("Content-Type", f.MIME)
	}
	http.ServeFile(w, r, f.Path)
}

func (s *Server) handleAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Attachments.Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "attachment_not_found", err.Error())
		return
	}
	s.publish("attachment.deleted", map[string]any{"id": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ---- container attachments ----

func (s *Server) requestEnvMeta(r *http.Request) (envdir.Meta, bool) {
	if name := r.PathValue("name"); name != "" {
		meta, _, ok := s.findEnvByName(name)
		return meta, ok
	}
	meta, _, ok := s.findEnv(r.PathValue("id"))
	return meta, ok
}

func (s *Server) containerAttachmentStore(meta envdir.Meta) *attachments.Store {
	meta.FillPaths()
	dir := meta.AttachmentDir
	if dir == "" {
		dir = filepath.Join(meta.Dir, "attachments")
	}
	return attachments.NewStore(dir, filepath.Join(dir, "attachments.json"))
}

func (s *Server) handleContainerAttachmentList(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.requestEnvMeta(r)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	store := s.containerAttachmentStore(meta)
	list, err := store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "attachment_list_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attachments": list, "root": store.Root()})
}

func (s *Server) handleContainerAttachmentAdd(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.requestEnvMeta(r)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAttachmentSize+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "file field is required")
		return
	}
	defer file.Close()
	name := r.FormValue("name")
	if name == "" && hdr != nil {
		name = hdr.Filename
	}
	saved, err := s.containerAttachmentStore(meta).Save(name, file, maxAttachmentSize)
	if err != nil {
		writeError(w, http.StatusBadRequest, "attachment_save_failed", err.Error())
		return
	}
	s.publish("attachment.created", saved)
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) handleContainerAttachmentGet(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.requestEnvMeta(r)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	f, err := s.containerAttachmentStore(meta).Get(r.PathValue("aid"))
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment_not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleContainerAttachmentRename(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.requestEnvMeta(r)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	f, err := s.containerAttachmentStore(meta).Rename(r.PathValue("aid"), in.Name)
	if err != nil {
		writeError(w, http.StatusBadRequest, "attachment_rename_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, f)
}

func (s *Server) handleContainerAttachmentDownload(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.requestEnvMeta(r)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	f, err := s.containerAttachmentStore(meta).Get(r.PathValue("aid"))
	if err != nil {
		writeError(w, http.StatusNotFound, "attachment_not_found", err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Name))
	if f.MIME != "" {
		w.Header().Set("Content-Type", f.MIME)
	}
	http.ServeFile(w, r, f.Path)
}

func (s *Server) handleContainerAttachmentDelete(w http.ResponseWriter, r *http.Request) {
	meta, ok := s.requestEnvMeta(r)
	if !ok {
		writeError(w, http.StatusNotFound, "env_not_found", "container not found")
		return
	}
	id := r.PathValue("aid")
	if err := s.containerAttachmentStore(meta).Delete(id); err != nil {
		writeError(w, http.StatusNotFound, "attachment_not_found", err.Error())
		return
	}
	s.publish("attachment.deleted", map[string]any{"id": id})
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ---- auth (cookies + user agent) ----

type AuthCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain,omitempty"`
	Path     string  `json:"path,omitempty"`
	URL      string  `json:"url,omitempty"`
	Secure   bool    `json:"secure,omitempty"`
	HTTPOnly bool    `json:"httpOnly,omitempty"`
	SameSite string  `json:"sameSite,omitempty"`
	Expires  float64 `json:"expires,omitempty"`
}

type AuthState struct {
	UserAgent      string       `json:"user_agent"`
	AcceptLanguage string       `json:"accept_language,omitempty"`
	Platform       string       `json:"platform,omitempty"`
	URL            string       `json:"url,omitempty"`
	Title          string       `json:"title,omitempty"`
	Cookies        []AuthCookie `json:"cookies"`
	ExportedAt     string       `json:"exported_at,omitempty"`
}

func (s *Server) buildAuthState(sess *session.Session) (AuthState, error) {
	st := AuthState{Cookies: []AuthCookie{}}
	err := sess.Do(func(ctx context.Context) error {
		st.URL, _ = browser.Location(ctx)
		st.Title, _ = browser.Title(ctx)
		if v, e := browser.Eval(ctx, "navigator.userAgent"); e == nil {
			if str, ok := v.(string); ok {
				st.UserAgent = str
			}
		}
		if v, e := browser.Eval(ctx, "navigator.language"); e == nil {
			if str, ok := v.(string); ok {
				st.AcceptLanguage = str
			}
		}
		if v, e := browser.Eval(ctx, "navigator.platform"); e == nil {
			if str, ok := v.(string); ok {
				st.Platform = str
			}
		}
		cookies, e := browser.GetCookies(ctx, nil)
		if e != nil {
			return e
		}
		for _, c := range cookies {
			st.Cookies = append(st.Cookies, AuthCookie{
				Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
				Secure: c.Secure, HTTPOnly: c.HTTPOnly, SameSite: string(c.SameSite),
				Expires: float64(c.Expires),
			})
		}
		return nil
	})
	st.ExportedAt = time.Now().Format(time.RFC3339)
	return st, err
}

func (s *Server) applyAuthState(sess *session.Session, st AuthState) error {
	return sess.Do(func(ctx context.Context) error {
		if st.UserAgent != "" {
			if err := browser.SetUserAgent(ctx, st.UserAgent, st.AcceptLanguage, st.Platform); err != nil {
				return err
			}
		}
		if len(st.Cookies) == 0 {
			return nil
		}
		current, _ := browser.Location(ctx)
		params := make([]*network.CookieParam, 0, len(st.Cookies))
		for _, c := range st.Cookies {
			if c.Name == "" {
				continue
			}
			p := &network.CookieParam{
				Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, URL: c.URL,
				Secure: c.Secure, HTTPOnly: c.HTTPOnly,
			}
			if c.SameSite != "" {
				p.SameSite = network.CookieSameSite(c.SameSite)
			}
			if c.Expires > 0 {
				p.Expires = cdp.TimeSinceEpoch(c.Expires)
			}
			if p.URL == "" && p.Domain == "" {
				p.URL = current
			}
			params = append(params, p)
		}
		if len(params) == 0 {
			return nil
		}
		return browser.SetCookies(ctx, params)
	})
}

func (s *Server) handleAuthGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	st, err := s.buildAuthState(sess)
	if err != nil {
		writeError(w, http.StatusBadGateway, "auth_export_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAuthExport(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	st, err := s.buildAuthState(sess)
	if err != nil {
		writeError(w, http.StatusBadGateway, "auth_export_failed", err.Error())
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "auth-"+sess.ID+".json"))
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handleAuthSet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	var st AuthState
	if !decode(w, r, &st) {
		return
	}
	if err := s.applyAuthState(sess, st); err != nil {
		writeError(w, http.StatusBadGateway, "auth_import_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAuthImport(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
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
	var st AuthState
	if err := json.NewDecoder(file).Decode(&st); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if err := s.applyAuthState(sess, st); err != nil {
		writeError(w, http.StatusBadGateway, "auth_import_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- browser access ----

func (s *Server) handleBrowserAccess(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>AutoChrome</title>
<h1>AutoChrome</h1>
<p>API: <code>/api/v1</code></p>
<p>VNC: <code>/vnc?token=&lt;api_token&gt;</code></p>`)
		return
	}
	s.tokenMu.RLock()
	current := s.Token
	s.tokenMu.RUnlock()
	if subtle.ConstantTimeCompare([]byte(token), []byte(current)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
		return
	}
	if s.Desktop == nil {
		writeError(w, http.StatusBadRequest, "vnc_disabled", "VNC is not enabled")
		return
	}
	if err := s.ensureBrowser(r.URL.Query().Get("env")); err != nil {
		writeError(w, http.StatusBadGateway, "browser_unavailable", err.Error())
		return
	}
	vt := s.newVNCToken(s.Desktop.Info().WebPort)
	http.Redirect(w, r, "/vnc/"+vt+"/vnc.html?autoconnect=1&resize=scale&reconnect=1&path=vnc/"+vt+"/websockify", http.StatusFound)
}

func (s *Server) ensureBrowser(envID string) error {
	envID = strings.TrimSpace(envID)
	if envID == "" {
		sessions := s.Manager.List()
		if len(sessions) > 0 {
			envID = sessions[len(sessions)-1].EnvID
		}
	}
	if envID == "" {
		envs := s.listEnvs()
		if len(envs) == 0 {
			return fmt.Errorf("no containers; create one first")
		}
		best := ""
		bestTime := ""
		for _, e := range envs {
			t := e.UpdatedAt
			if t == "" {
				t = e.CreatedAt
			}
			if t > bestTime {
				bestTime, best = t, e.ID
			}
		}
		envID = best
	}
	if existing, ok := s.Manager.SessionForEnv(envID); ok {
		err := existing.Do(func(ctx context.Context) error {
			loc, e := browser.Location(ctx)
			if e != nil {
				return e
			}
			if strings.TrimSpace(loc) == "" || loc == "about:blank" {
				return fmt.Errorf("no active page")
			}
			return nil
		})
		if err == nil {
			return nil
		}
		_, err = s.Manager.Restart(existing.ID, "")
		return err
	}
	_, err := s.Manager.Open(envID, "", "about:blank", "", false, "")
	return err
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
	w.Header().Set("Cache-Control", "no-store")
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
// SaveToken writes a new token for the user.
func SaveToken(u users.User, token string) error {
	path := TokenPath(u)
	lock, err := filelock.Acquire(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	return atomicfile.Write(path, []byte(token+"\n"), 0o600)
}

func LoadOrCreateToken(u users.User) (string, error) {
	path := TokenPath(u)
	lock, err := filelock.Acquire(path)
	if err != nil {
		return "", err
	}
	defer lock.Close()
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
	if err := atomicfile.Write(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}
