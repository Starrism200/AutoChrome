// Package mcpserver exposes AutoChrome as a Model Context Protocol server over
// stdio. One server process is bound to one user, which is the L2 isolation
// model: separate process, separate directories, separate browser slots.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"autochrome/internal/browser"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
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
}

// New returns a server for user.
func New(u users.User, chrome string) *Server {
	return &Server{
		User:         u,
		Chrome:       chrome,
		AllowEval:    u.AllowEval,
		AllowScripts: u.AllowScripts,
	}
}

// Run starts the stdio MCP server and blocks until the client disconnects.
func (s *Server) Run(ctx context.Context) error {
	s.Manager = session.NewManager(s.User, s.Chrome)
	defer s.Manager.CloseAll()

	srv := mcp.NewServer(
		&mcp.Implementation{Name: "autochrome", Title: "AutoChrome", Version: Version},
		&mcp.ServerOptions{Instructions: instructions},
	)
	s.register(srv)
	return srv.Run(ctx, &mcp.StdioTransport{})
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
embedded JavaScript runtime with a synchronous API.`

func (s *Server) register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{Name: "env_create", Description: "Create a browser environment directory with a deterministic fingerprint."}, s.envCreate)
	mcp.AddTool(srv, &mcp.Tool{Name: "env_list", Description: "List browser environments for this user."}, s.envList)
	mcp.AddTool(srv, &mcp.Tool{Name: "browser_open", Description: "Start a browser session for an environment."}, s.browserOpen)
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
	if s.AllowEval {
		mcp.AddTool(srv, &mcp.Tool{Name: "browser_eval", Description: "Evaluate JavaScript in the page and await promises."}, s.browserEval)
	}
	if s.AllowScripts {
		mcp.AddTool(srv, &mcp.Tool{Name: "script_list", Description: "List reusable JavaScript scripts for this user."}, s.scriptList)
		mcp.AddTool(srv, &mcp.Tool{Name: "script_run", Description: "Run a reusable JavaScript script with JSON input."}, s.scriptRun)
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
	dir, err := envdir.Create(s.User.EnvRoot, platform, in.ID, fp)
	if err != nil {
		return nil, EnvCreateOut{}, err
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
	out := []envdir.Meta{}
	entries, err := os.ReadDir(s.User.EnvRoot)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.User.EnvRoot, e.Name())
		if meta, ok := envdir.ReadMeta(dir); ok {
			out = append(out, meta)
		}
	}
	return out
}

// ---- browser lifecycle ----

type BrowserOpenIn struct {
	Env      string `json:"env" jsonschema:"environment id"`
	Platform string `json:"platform,omitempty" jsonschema:"directory prefix (default: default)"`
	URL      string `json:"url,omitempty" jsonschema:"initial page to open"`
	Proxy    string `json:"proxy,omitempty" jsonschema:"proxy address, e.g. socks5://127.0.0.1:10801"`
	Headless bool   `json:"headless,omitempty" jsonschema:"run Chrome headless instead of on Xvfb"`
}

type BrowserOpenOut struct {
	SessionID string `json:"session_id"`
	Env       string `json:"env"`
	URL       string `json:"url,omitempty"`
	Title     string `json:"title,omitempty"`
}

func (s *Server) browserOpen(_ context.Context, _ *mcp.CallToolRequest, in BrowserOpenIn) (*mcp.CallToolResult, BrowserOpenOut, error) {
	sess, err := s.Manager.Open(in.Env, in.Platform, in.URL, in.Proxy, in.Headless)
	if err != nil {
		return nil, BrowserOpenOut{}, err
	}
	out := BrowserOpenOut{SessionID: sess.ID, Env: sess.EnvID, URL: in.URL}
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
	SessionID string `json:"session_id" jsonschema:"session id"`
	Selector  string `json:"selector,omitempty" jsonschema:"CSS selector to wait for"`
	Text      string `json:"text,omitempty" jsonschema:"visible text to wait for"`
	TimeoutMS int    `json:"timeout_ms,omitempty" jsonschema:"timeout in milliseconds (default 30000)"`
}

func (s *Server) browserWait(_ context.Context, _ *mcp.CallToolRequest, in WaitIn) (*mcp.CallToolResult, OKOut, error) {
	sess, err := s.Manager.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, err
	}
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	if err := sess.Do(func(ctx context.Context) error {
		return browser.WaitFor(ctx, in.Selector, in.Text, timeout)
	}); err != nil {
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
			path = filepath.Join(s.User.ShotRoot, path)
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

// ---- script tools ----

type ScriptListOut struct {
	Scripts []scripts.Script `json:"scripts"`
}

func (s *Server) scriptList(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ScriptListOut, error) {
	list, err := scripts.List(s.User.ScriptRoot)
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
	script, src, err := scripts.Load(s.User.ScriptRoot, in.Name)
	if err != nil {
		return nil, nil, err
	}
	runner := jsruntime.New(s.User, s.Manager, os.Stderr)
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
