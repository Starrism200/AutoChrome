// Package mcpserver exposes AutoChrome as a Model Context Protocol server over
// stdio. One server process is bound to one user, which is the L2 isolation
// model: separate process, separate directories, separate browser slots.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"autochrome/internal/browser"
	"autochrome/internal/desktop"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
	"autochrome/internal/jobs"
	"autochrome/internal/jsruntime"
	"autochrome/internal/scripts"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

// Version is reported to MCP clients.
const Version = "0.2.0"

// Server is an MCP server bound to one user.
type Server struct {
	User         users.User
	Chrome       string
	AllowEval    bool
	AllowScripts bool
	Manager      *session.Manager
	Desktop      *desktop.Desktop
	Registry     *envdir.Registry
	Jobs         *jobs.Manager
	mcpServer    *mcp.Server
	buildOnce    sync.Once
	userMu       sync.RWMutex
}

// New returns a server for user.
func New(u users.User, chrome string) *Server {
	return &Server{
		User:         u,
		Chrome:       chrome,
		AllowEval:    u.AllowEval,
		AllowScripts: u.AllowScripts,
		Registry:     envdir.NewRegistry(u.RegistryPath()),
	}
}

// SetUser updates storage-related user settings.
func (s *Server) SetUser(u users.User) {
	s.userMu.Lock()
	s.User = u
	s.userMu.Unlock()
}

func (s *Server) userSnapshot() users.User {
	s.userMu.RLock()
	defer s.userMu.RUnlock()
	return s.User
}

// Build creates (once) the MCP server with all tools and resources.
func (s *Server) Build() *mcp.Server {
	if s.mcpServer != nil {
		return s.mcpServer
	}
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "autochrome", Title: "AutoChrome", Version: Version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	s.register(srv)
	s.mcpServer = srv
	return srv
}

// Handler returns the Streamable HTTP handler for remote MCP clients.
func (s *Server) Handler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return s.Build()
	}, &mcp.StreamableHTTPOptions{})
}

// Run starts the stdio MCP server and blocks until the client disconnects.
func (s *Server) Run(ctx context.Context) error {
	if s.Manager == nil {
		u := s.userSnapshot()
		s.Manager = session.NewManager(u, s.Chrome)
		if s.Desktop != nil {
			s.Manager.SetDisplay(s.Desktop.Display())
		}
		defer s.Manager.CloseAll()
	}
	if s.Jobs == nil {
		s.Jobs = jobs.New(s.userSnapshot(), s.Manager, s.AllowEval, nil)
		defer s.Jobs.Close()
	}
	return s.Build().Run(ctx, &mcp.StdioTransport{})
}

const instructions = `AutoChrome drives Google Chrome on a virtual display with persistent, per-environment profiles.

Workflow:
1. Use env_create to create an environment (or env_list to reuse one).
2. Use browser_open to start a session for an environment.
3. Call browser_snapshot to read the page as text with element refs like e1, e2.
4. Use browser_click / browser_type / browser_press with those refs.
5. Use browser_screenshot when a visual check is needed.
6. Use browser_close when done.

Sessions stay alive between tool calls. Prefer snapshot refs over CSS selectors.
Use script_list / script_run for reusable scripts. Scripts are executed in an
embedded JavaScript runtime with a synchronous API.

If a QR code, OTP input or CAPTCHA appears, call browser_detect_challenge, then
browser_qr or browser_screenshot. When desktop_info reports a noVNC URL, tell
the human to take over there. Wait for completion with browser_wait using
url_contains, cookie_name, hidden_selector or text.`

func (s *Server) register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "env_create", Description: "Create a browser environment directory with a deterministic fingerprint."}, s.envCreate)
	mcp.AddTool(srv, &mcp.Tool{Name: "env_list", Description: "List browser environments for this user."}, s.envList)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_open", Description: "Start a browser session for an environment."}, s.browserOpen)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_restart", Description: "Restart a session or environment browser (e.g. after closing the last tab)."}, s.browserRestart)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_close", Description: "Close a browser session."}, s.browserClose)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_list", Description: "List live browser sessions."}, s.browserList)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_navigate", Description: "Navigate a session to a URL."}, s.browserNavigate)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_snapshot", Description: "Return a compact text snapshot of the page with element refs."}, s.browserSnapshot)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_click", Description: "Click an element by snapshot ref, CSS selector or visible text."}, s.browserClick)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_type", Description: "Focus an element and type text. Optionally press Enter."}, s.browserType)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_press", Description: "Press a key on the focused element."}, s.browserPress)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_wait", Description: "Wait for a selector or visible text."}, s.browserWait)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_screenshot", Description: "Capture a PNG screenshot, optionally inline."}, s.browserScreenshot)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_cookies", Description: "Return cookies for the session."}, s.browserCookies)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_detect_challenge", Description: "Detect QR code, OTP and CAPTCHA candidates on the page."}, s.browserDetectChallenge)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_qr", Description: "Capture the first detected QR code as an inline image for a human to scan."}, s.browserQR)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_click_xy", Description: "Click at viewport coordinates (for image CAPTCHAs)."}, s.browserClickXY)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_scroll", Description: "Scroll the page by dx/dy CSS pixels."}, s.browserScroll)
	mcp.AddTool(srv, &mcp.Tool{Name: "desktop_info", Description: "Return the noVNC URL for human takeover, when enabled."}, s.desktopInfo)
	if s.AllowEval {
		mcp.AddTool(srv, &mcp.Tool{Name: "browser_eval", Description: "Evaluate JavaScript in the page and await promises."}, s.browserEval)
	}
	if s.AllowScripts {
		mcp.AddTool(srv, &mcp.Tool{Name: "script_list", Description: "List reusable JavaScript scripts for this user."}, s.scriptList)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_run", Description: "Run a reusable JavaScript script synchronously."}, s.scriptRun)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_job_start", Description: "Start an asynchronous script job and return a job id."}, s.scriptJobStart)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_job_status", Description: "Get an asynchronous script job status and result."}, s.scriptJobStatus)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_job_logs", Description: "Read an asynchronous script job log."}, s.scriptJobLogs)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_job_cancel", Description: "Cancel an asynchronous script job."}, s.scriptJobCancel)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_job_list", Description: "List asynchronous script jobs."}, s.scriptJobList)
	}

	srv.AddResource(&mcp.Resource{
		URI: "autochrome://envs", Name: "environments", Title: "Browser environments",
		Description: "Environments owned by this user", MIMEType: "application/json",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		data, _ := json.MarshalIndent(s.listEnvs(), "", "  ")
		return resourceText(req.Params.URI, string(data)), nil
	})
	srv.AddResource(&mcp.Resource{
		URI: "autochrome://sessions", Name: "sessions", Title: "Live browser sessions",
		Description: "Sessions owned by this user", MIMEType: "application/json",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		data, _ := json.MarshalIndent(s.Manager.List(), "", "  ")
		return resourceText(req.Params.URI, string(data)), nil
	})
	srv.AddResource(&mcp.Resource{
		URI: "autochrome://jobs", Name: "jobs", Title: "Script jobs",
		Description: "Asynchronous script jobs for this user", MIMEType: "application/json",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		var list []jobs.Job
		if s.Jobs != nil {
			list = s.Jobs.List()
		}
		data, _ := json.MarshalIndent(list, "", "  ")
		return resourceText(req.Params.URI, string(data)), nil
	})
	srv.AddResource(&mcp.Resource{
		URI: "autochrome://desktop", Name: "desktop", Title: "VNC desktop",
		Description: "noVNC endpoint for human takeover", MIMEType: "application/json",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		data, _ := json.MarshalIndent(s.desktopInfoValue(), "", "  ")
		return resourceText(req.Params.URI, string(data)), nil
	})
}

func resourceText(uri, text string) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI: uri, MIMEType: "application/json", Text: text,
	}}}
}

// ---- env tools ----

type EnvCreateIn struct {
	ID       string `json:"id" jsonschema:"environment id"`
	Platform string `json:"platform,omitempty" jsonschema:"directory prefix (default: default)"`
}

type EnvCreateOut struct {
	Dir         string              `json:"dir"`
	Fingerprint fingerprint.Profile `json:"fingerprint"`
}

func (s *Server) envCreate(_ context.Context, _ *mcp.CallToolRequest, in EnvCreateIn) (*mcp.CallToolResult, EnvCreateOut, error) {
	if in.ID == "" {
		return nil, EnvCreateOut{}, fmt.Errorf("id is required")
	}
	platform := in.Platform
	if platform == "" {
		platform = "default"
	}
	fp := fingerprint.Generate(in.ID)
	user := s.userSnapshot()
	dir, err := envdir.Create(user.EnvRoot, platform, in.ID, fp)
	if err != nil {
		return nil, EnvCreateOut{}, err
	}
	if meta, ok := envdir.ReadMeta(dir); ok {
		_ = s.Registry.Put(meta)
	}
	return nil, EnvCreateOut{Dir: dir, Fingerprint: fp}, nil
}

type EnvListOut struct {
	Envs []envdir.Meta `json:"envs"`
}

func (s *Server) envList(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, EnvListOut, error) {
	return nil, EnvListOut{Envs: s.listEnvs()}, nil
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

// ---- browser lifecycle ----

type BrowserOpenIn struct {
	Env           string `json:"env" jsonschema:"environment id"`
	Platform      string `json:"platform,omitempty" jsonschema:"directory prefix (default: default)"`
	URL           string `json:"url,omitempty" jsonschema:"initial page to open"`
	Proxy         string `json:"proxy,omitempty" jsonschema:"proxy address, e.g. socks5://127.0.0.1:10801"`
	ChromeVersion string `json:"chrome_version,omitempty" jsonschema:"managed Chrome version, e.g. 156.0.8078.79"`
	Headless      bool   `json:"headless,omitempty" jsonschema:"run Chrome headless instead of on Xvfb"`
}

type BrowserRestartIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"session id to restart"`
	Env       string `json:"env,omitempty" jsonschema:"environment id to restart or open"`
	URL       string `json:"url,omitempty" jsonschema:"optional page URL; defaults to the last URL"`
}

type BrowserOpenOut struct {
	SessionID     string `json:"session_id"`
	Env           string `json:"env"`
	URL           string `json:"url,omitempty"`
	Title         string `json:"title,omitempty"`
	ChromeVersion string `json:"chrome_version,omitempty"`
}

func (s *Server) browserOpen(_ context.Context, _ *mcp.CallToolRequest, in BrowserOpenIn) (*mcp.CallToolResult, BrowserOpenOut, error) {
	sess, err := s.Manager.Open(in.Env, in.Platform, in.URL, in.Proxy, in.Headless, in.ChromeVersion)
	if err != nil {
		return nil, BrowserOpenOut{}, err
	}
	info := sess.Info()
	out := BrowserOpenOut{SessionID: sess.ID, Env: sess.EnvID, URL: in.URL, ChromeVersion: info.ChromeVersion}
	_ = sess.Do(func(ctx context.Context) error {
		out.URL, _ = browser.Location(ctx)
		out.Title, _ = browser.Title(ctx)
		return nil
	})
	return nil, out, nil
}

func (s *Server) browserRestart(_ context.Context, _ *mcp.CallToolRequest, in BrowserRestartIn) (*mcp.CallToolResult, BrowserOpenOut, error) {
	var (
		sess *session.Session
		err  error
	)
	switch {
	case in.SessionID != "":
		sess, err = s.Manager.Restart(in.SessionID, in.URL)
	case in.Env != "":
		sess, err = s.Manager.RestartEnv(in.Env, in.URL)
	default:
		return nil, BrowserOpenOut{}, fmt.Errorf("session_id or env is required")
	}
	if err != nil {
		return nil, BrowserOpenOut{}, err
	}
	info := sess.Info()
	out := BrowserOpenOut{
		SessionID: sess.ID, Env: sess.EnvID, URL: info.URL, ChromeVersion: info.ChromeVersion,
	}
	_ = sess.Do(func(ctx context.Context) error {
		out.URL, _ = browser.Location(ctx)
		out.Title, _ = browser.Title(ctx)
		return nil
	})
	return nil, out, nil
}

type SessionIn struct {
	SessionID string `json:"session_id" jsonschema:"session id returned by browser_open"`
}

type BrowserCloseOut struct {
	Closed bool `json:"closed"`
}

func (s *Server) browserClose(_ context.Context, _ *mcp.CallToolRequest, in SessionIn) (*mcp.CallToolResult, BrowserCloseOut, error) {
	if err := s.Manager.Close(in.SessionID); err != nil {
		return nil, BrowserCloseOut{}, err
	}
	return nil, BrowserCloseOut{Closed: true}, nil
}

type BrowserListOut struct {
	Sessions []session.Info `json:"sessions"`
}

func (s *Server) browserList(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, BrowserListOut, error) {
	return nil, BrowserListOut{Sessions: s.Manager.List()}, nil
}

// ---- page tools ----

type NavigateIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	URL       string `json:"url" jsonschema:"page URL"`
}

type NavigateOut struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

func (s *Server) browserNavigate(_ context.Context, _ *mcp.CallToolRequest, in NavigateIn) (*mcp.CallToolResult, NavigateOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, NavigateOut{}, err
	}
	var out NavigateOut
	err = sess.Do(func(ctx context.Context) error {
		if err := browser.Navigate(ctx, in.URL); err != nil {
			return err
		}
		out.URL, _ = browser.Location(ctx)
		out.Title, _ = browser.Title(ctx)
		return nil
	})
	if err != nil {
		return nil, NavigateOut{}, err
	}
	sess.SetURL(out.URL)
	return nil, out, nil
}

type SnapshotIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	Max       int    `json:"max,omitempty" jsonschema:"maximum number of nodes (default 300)"`
}

type SnapshotOut struct {
	URL      string                 `json:"url"`
	Title    string                 `json:"title"`
	Snapshot string                 `json:"snapshot"`
	Nodes    []browser.SnapshotNode `json:"nodes"`
}

func (s *Server) browserSnapshot(_ context.Context, _ *mcp.CallToolRequest, in SnapshotIn) (*mcp.CallToolResult, SnapshotOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, SnapshotOut{}, err
	}
	var snap browser.Snapshot
	err = sess.Do(func(ctx context.Context) error {
		v, e := browser.TakeSnapshot(ctx, in.Max)
		snap = v
		return e
	})
	if err != nil {
		return nil, SnapshotOut{}, err
	}
	sess.SetURL(snap.URL)
	return nil, SnapshotOut{
		URL: snap.URL, Title: snap.Title, Snapshot: browser.SnapshotText(snap), Nodes: snap.Nodes,
	}, nil
}

type ClickIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	Ref       string `json:"ref,omitempty" jsonschema:"snapshot element ref, e.g. e3"`
	Selector  string `json:"selector,omitempty" jsonschema:"CSS selector"`
	Text      string `json:"text,omitempty" jsonschema:"visible text of the element"`
}

type OKOut struct {
	OK bool `json:"ok"`
}

func (s *Server) browserClick(_ context.Context, _ *mcp.CallToolRequest, in ClickIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	target := browser.Target{Ref: in.Ref, Selector: in.Selector, Text: in.Text}
	if err := sess.Do(func(ctx context.Context) error { return browser.Click(ctx, target) }); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true}, nil
}

type TypeIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	Ref       string `json:"ref,omitempty" jsonschema:"snapshot element ref"`
	Selector  string `json:"selector,omitempty" jsonschema:"CSS selector"`
	Text      string `json:"text" jsonschema:"text to type"`
	Submit    bool   `json:"submit,omitempty" jsonschema:"press Enter after typing"`
}

func (s *Server) browserType(_ context.Context, _ *mcp.CallToolRequest, in TypeIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	target := browser.Target{Ref: in.Ref, Selector: in.Selector}
	if err := sess.Do(func(ctx context.Context) error { return browser.Type(ctx, target, in.Text) }); err != nil {
		return nil, OKOut{}, err
	}
	if in.Submit {
		if err := sess.Do(func(ctx context.Context) error { return browser.Press(ctx, "\r") }); err != nil {
			return nil, OKOut{}, err
		}
	}
	return nil, OKOut{OK: true}, nil
}

type PressIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	Key       string `json:"key" jsonschema:"key name, e.g. Enter, Tab, Escape, Backspace"`
}

func (s *Server) browserPress(_ context.Context, _ *mcp.CallToolRequest, in PressIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.Press(ctx, in.Key) }); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true}, nil
}

type WaitIn struct {
	SessionID      string `json:"session_id" jsonschema:"session id"`
	Selector       string `json:"selector,omitempty" jsonschema:"CSS selector to wait for"`
	HiddenSelector string `json:"hidden_selector,omitempty" jsonschema:"CSS selector that must disappear"`
	Text           string `json:"text,omitempty" jsonschema:"visible text to wait for"`
	URLContains    string `json:"url_contains,omitempty" jsonschema:"substring the page URL must contain"`
	CookieName     string `json:"cookie_name,omitempty" jsonschema:"cookie name that must exist"`
	TimeoutMS      int    `json:"timeout_ms,omitempty" jsonschema:"timeout in milliseconds (default 30000)"`
}

func (s *Server) browserWait(_ context.Context, _ *mcp.CallToolRequest, in WaitIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	opts := browser.WaitOptions{
		Selector: in.Selector, HiddenSelector: in.HiddenSelector, Text: in.Text,
		URLContains: in.URLContains, CookieName: in.CookieName,
		Timeout: time.Duration(in.TimeoutMS) * time.Millisecond,
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.WaitForOptions(ctx, opts) }); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true}, nil
}

type ScreenshotIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	Path      string `json:"path,omitempty" jsonschema:"file path (relative paths are under the user screenshots dir)"`
	FullPage  bool   `json:"full_page,omitempty" jsonschema:"capture the full page"`
	Inline    bool   `json:"inline,omitempty" jsonschema:"return the PNG inline as image content"`
}

type ScreenshotOut struct {
	Path  string `json:"path,omitempty"`
	Bytes int    `json:"bytes"`
}

func (s *Server) browserScreenshot(_ context.Context, _ *mcp.CallToolRequest, in ScreenshotIn) (*mcp.CallToolResult, ScreenshotOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, ScreenshotOut{}, err
	}
	var buf []byte
	if err := sess.Do(func(ctx context.Context) error {
		b, e := browser.CapturePNG(ctx, in.FullPage)
		buf = b
		return e
	}); err != nil {
		return nil, ScreenshotOut{}, err
	}
	out := ScreenshotOut{Bytes: len(buf)}
	if in.Path != "" {
		path := in.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.userSnapshot().ShotRoot, path)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, ScreenshotOut{}, err
		}
		if err := os.WriteFile(path, buf, 0o644); err != nil {
			return nil, ScreenshotOut{}, err
		}
		out.Path = path
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("captured %d bytes", len(buf))}}}
	if in.Inline || in.Path == "" {
		res.Content = append(res.Content, &mcp.ImageContent{Data: buf, MIMEType: "image/png"})
	}
	return res, out, nil
}

type CookiesIn struct {
	SessionID string   `json:"session_id" jsonschema:"session id"`
	URLs      []string `json:"urls,omitempty" jsonschema:"cookie URLs (empty returns all cookies)"`
}

func (s *Server) browserCookies(_ context.Context, _ *mcp.CallToolRequest, in CookiesIn) (*mcp.CallToolResult, any, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	var cookies any
	if err := sess.Do(func(ctx context.Context) error {
		c, e := browser.GetCookies(ctx, in.URLs)
		cookies = c
		return e
	}); err != nil {
		return nil, nil, err
	}
	return nil, cookies, nil
}

type EvalIn struct {
	SessionID  string `json:"session_id" jsonschema:"session id"`
	Expression string `json:"expression" jsonschema:"JavaScript expression; promises are awaited"`
}

func (s *Server) browserEval(_ context.Context, _ *mcp.CallToolRequest, in EvalIn) (*mcp.CallToolResult, any, error) {
	if !s.AllowEval {
		return nil, nil, fmt.Errorf("eval is disabled for this user")
	}
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, nil, err
	}
	var out any
	if err := sess.Do(func(ctx context.Context) error {
		v, e := browser.Eval(ctx, in.Expression)
		out = v
		return e
	}); err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

// ---- interactive takeover helpers ----

type DetectChallengeIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
}

type DetectChallengeOut struct {
	Challenges []browser.ChallengeCandidate `json:"challenges"`
}

func (s *Server) browserDetectChallenge(_ context.Context, _ *mcp.CallToolRequest, in DetectChallengeIn) (*mcp.CallToolResult, DetectChallengeOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, DetectChallengeOut{}, err
	}
	var out []browser.ChallengeCandidate
	if err := sess.Do(func(ctx context.Context) error {
		v, e := browser.DetectChallenges(ctx)
		out = v
		return e
	}); err != nil {
		return nil, DetectChallengeOut{}, err
	}
	return nil, DetectChallengeOut{Challenges: out}, nil
}

type QRIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
}

type QROut struct {
	Selector string `json:"selector"`
	Name     string `json:"name,omitempty"`
	Bytes    int    `json:"bytes"`
}

func (s *Server) browserQR(_ context.Context, _ *mcp.CallToolRequest, in QRIn) (*mcp.CallToolResult, QROut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, QROut{}, err
	}
	var buf []byte
	var cand browser.ChallengeCandidate
	if err := sess.Do(func(ctx context.Context) error {
		b, c, e := browser.QRCodePNG(ctx)
		buf, cand = b, c
		return e
	}); err != nil {
		return nil, QROut{}, err
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.TextContent{Text: "QR code captured for human scanning"},
		&mcp.ImageContent{Data: buf, MIMEType: "image/png"},
	}}
	return res, QROut{Selector: cand.Selector, Name: cand.Name, Bytes: len(buf)}, nil
}

type ClickXYIn struct {
	SessionID string  `json:"session_id" jsonschema:"session id"`
	X         float64 `json:"x" jsonschema:"viewport x in CSS pixels"`
	Y         float64 `json:"y" jsonschema:"viewport y in CSS pixels"`
}

func (s *Server) browserClickXY(_ context.Context, _ *mcp.CallToolRequest, in ClickXYIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.ClickXY(ctx, in.X, in.Y) }); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true}, nil
}

type ScrollIn struct {
	SessionID string `json:"session_id" jsonschema:"session id"`
	DX        int    `json:"dx,omitempty" jsonschema:"horizontal delta in CSS pixels"`
	DY        int    `json:"dy,omitempty" jsonschema:"vertical delta in CSS pixels"`
}

func (s *Server) browserScroll(_ context.Context, _ *mcp.CallToolRequest, in ScrollIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	if err := sess.Do(func(ctx context.Context) error { return browser.Scroll(ctx, in.DX, in.DY) }); err != nil {
		return nil, OKOut{}, err
	}
	return nil, OKOut{OK: true}, nil
}

type DesktopInfoOut struct {
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url,omitempty"`
	Display  string `json:"display,omitempty"`
	RFBPort  int    `json:"rfb_port,omitempty"`
	WebPort  int    `json:"web_port,omitempty"`
	Password bool   `json:"password,omitempty"`
}

func (s *Server) desktopInfoValue() DesktopInfoOut {
	if s.Desktop == nil {
		return DesktopInfoOut{Enabled: false}
	}
	info := s.Desktop.Info()
	return DesktopInfoOut{
		Enabled: true, URL: info.URL, Display: info.Display,
		RFBPort: info.RFBPort, WebPort: info.WebPort, Password: info.Password,
	}
}

func (s *Server) desktopInfo(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, DesktopInfoOut, error) {
	return nil, s.desktopInfoValue(), nil
}

// ---- script tools ----

type ScriptListOut struct {
	Scripts []scripts.Script `json:"scripts"`
}

func (s *Server) scriptList(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ScriptListOut, error) {
	list, err := scripts.List(s.userSnapshot().ScriptRoot)
	if err != nil {
		return nil, ScriptListOut{}, err
	}
	return nil, ScriptListOut{Scripts: list}, nil
}

type ScriptRunIn struct {
	Name      string         `json:"name" jsonschema:"script name (file name without .js)"`
	Input     map[string]any `json:"input,omitempty" jsonschema:"JSON input passed to the script"`
	SessionID string         `json:"session_id,omitempty" jsonschema:"existing session id to expose as input.session_id"`
}

func (s *Server) scriptRun(_ context.Context, _ *mcp.CallToolRequest, in ScriptRunIn) (*mcp.CallToolResult, any, error) {
	if !s.AllowScripts {
		return nil, nil, fmt.Errorf("scripts are disabled for this user")
	}
	script, src, err := scripts.Load(s.userSnapshot().ScriptRoot, in.Name)
	if err != nil {
		return nil, nil, err
	}
	runner := jsruntime.New(s.userSnapshot(), s.Manager, os.Stderr)
	runner.AllowEval = s.AllowEval
	runner.AutoClose = true
	runner.Timeout = script.TimeoutDuration()
	input := in.Input
	if input == nil {
		input = map[string]any{}
	}
	if in.SessionID != "" {
		input["session_id"] = in.SessionID
	}
	result, err := runner.Run(src, input)
	if err != nil {
		return nil, nil, err
	}
	return nil, result, nil
}

// ---- async script jobs ----

type ScriptJobStartIn struct {
	Name      string         `json:"name" jsonschema:"script name"`
	Input     map[string]any `json:"input,omitempty" jsonschema:"JSON input passed to the script"`
	SessionID string         `json:"session_id,omitempty" jsonschema:"existing session id exposed as input.session_id"`
}

type ScriptJobStartOut struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
}

func (s *Server) scriptJobStart(_ context.Context, _ *mcp.CallToolRequest, in ScriptJobStartIn) (*mcp.CallToolResult, ScriptJobStartOut, error) {
	if !s.AllowScripts {
		return nil, ScriptJobStartOut{}, fmt.Errorf("scripts are disabled for this user")
	}
	if s.Jobs == nil {
		return nil, ScriptJobStartOut{}, fmt.Errorf("job manager is not ready")
	}
	job, err := s.Jobs.Start(in.Name, in.Input, in.SessionID)
	if err != nil {
		return nil, ScriptJobStartOut{}, err
	}
	return nil, ScriptJobStartOut{JobID: job.ID, Status: job.Status}, nil
}

type JobIDIn struct {
	JobID string `json:"job_id" jsonschema:"job id"`
}

func (s *Server) scriptJobStatus(_ context.Context, _ *mcp.CallToolRequest, in JobIDIn) (*mcp.CallToolResult, jobs.Job, error) {
	if s.Jobs == nil {
		return nil, jobs.Job{}, fmt.Errorf("job manager is not ready")
	}
	job, ok := s.Jobs.Get(in.JobID)
	if !ok {
		return nil, jobs.Job{}, fmt.Errorf("job %q not found", in.JobID)
	}
	return nil, job, nil
}

type ScriptJobLogsOut struct {
	Logs string `json:"logs"`
}

func (s *Server) scriptJobLogs(_ context.Context, _ *mcp.CallToolRequest, in JobIDIn) (*mcp.CallToolResult, ScriptJobLogsOut, error) {
	if s.Jobs == nil {
		return nil, ScriptJobLogsOut{}, fmt.Errorf("job manager is not ready")
	}
	logs, err := s.Jobs.Logs(in.JobID)
	if err != nil {
		return nil, ScriptJobLogsOut{}, err
	}
	return nil, ScriptJobLogsOut{Logs: logs}, nil
}

type ScriptJobCancelOut struct {
	Cancelled bool `json:"cancelled"`
}

func (s *Server) scriptJobCancel(_ context.Context, _ *mcp.CallToolRequest, in JobIDIn) (*mcp.CallToolResult, ScriptJobCancelOut, error) {
	if s.Jobs == nil {
		return nil, ScriptJobCancelOut{}, fmt.Errorf("job manager is not ready")
	}
	if err := s.Jobs.Cancel(in.JobID); err != nil {
		return nil, ScriptJobCancelOut{}, err
	}
	return nil, ScriptJobCancelOut{Cancelled: true}, nil
}

type ScriptJobListOut struct {
	Jobs []jobs.Job `json:"jobs"`
}

func (s *Server) scriptJobList(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ScriptJobListOut, error) {
	if s.Jobs == nil {
		return nil, ScriptJobListOut{}, fmt.Errorf("job manager is not ready")
	}
	return nil, ScriptJobListOut{Jobs: s.Jobs.List()}, nil
}
