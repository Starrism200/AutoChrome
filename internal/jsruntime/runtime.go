// Package jsruntime embeds a synchronous JavaScript SDK for generated browser
// scripts. Scripts receive an `ac` object, an `input` value and a `console`;
// they return a JSON-serializable value.
package jsruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"

	"autochrome/internal/browser"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

var refPattern = regexp.MustCompile(`^e[0-9]+$`)

// Runner executes JavaScript scripts against one user's sessions.
type Runner struct {
	User       users.User
	Manager    *session.Manager
	Registry   *envdir.Registry
	Log        io.Writer
	AllowEval  bool
	AutoClose  bool
	Timeout    time.Duration
	opened     []string
	openMarker map[string]bool
}

// New returns a runner. Log defaults to os.Stderr.
func New(user users.User, mgr *session.Manager, logw io.Writer) *Runner {
	if logw == nil {
		logw = os.Stderr
	}
	return &Runner{
		User: user, Manager: mgr, Log: logw, Timeout: 60 * time.Second,
		Registry: envdir.NewRegistry(user.RegistryPath()),
	}
}

// Run executes script with input and returns its exported result.
func (r *Runner) Run(script string, input map[string]any) (any, error) {
	return r.RunContext(context.Background(), script, input)
}

// RunContext is Run with cancellation and timeout support.
func (r *Runner) RunContext(ctx context.Context, script string, input map[string]any) (result any, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	r.opened = nil
	r.openMarker = map[string]bool{}
	vm := goja.New()
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	console := vm.NewObject()
	_ = console.Set("log", func(call goja.FunctionCall) goja.Value { r.log(call); return goja.Undefined() })
	_ = console.Set("error", func(call goja.FunctionCall) goja.Value { r.log(call); return goja.Undefined() })
	_ = console.Set("warn", func(call goja.FunctionCall) goja.Value { r.log(call); return goja.Undefined() })

	ac := r.buildAC(vm)

	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("script panic: %v", rec)
		}
		if r.AutoClose {
			for _, id := range r.opened {
				_ = r.Manager.Close(id)
			}
		}
	}()

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt("script timeout")
	})
	defer timer.Stop()

	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt("script cancelled")
		case <-stop:
		}
	}()
	defer close(stop)

	wrapped := "(function(input, ac, console) {\n" + script + "\n})"
	v, err := vm.RunString(wrapped)
	if err != nil {
		return nil, err
	}
	fn, ok := goja.AssertFunction(v)
	if !ok {
		return nil, fmt.Errorf("script did not compile to a function")
	}
	res, err := fn(goja.Undefined(), vm.ToValue(input), ac, console)
	if err != nil {
		return nil, err
	}
	if goja.IsUndefined(res) || goja.IsNull(res) {
		return nil, nil
	}
	return res.Export(), nil
}

func (r *Runner) log(call goja.FunctionCall) {
	parts := make([]string, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		if a == nil || goja.IsUndefined(a) {
			parts = append(parts, "undefined")
			continue
		}
		if s, ok := a.Export().(string); ok {
			parts = append(parts, s)
			continue
		}
		b, err := json.Marshal(a.Export())
		if err != nil {
			parts = append(parts, a.String())
			continue
		}
		parts = append(parts, string(b))
	}
	fmt.Fprintln(r.Log, strings.Join(parts, " "))
}

func (r *Runner) buildAC(vm *goja.Runtime) *goja.Object {
	ac := vm.NewObject()

	_ = ac.Set("open", func(call goja.FunctionCall) goja.Value {
		opts := objectArg(call, 0)
		id := stringOpt(opts, "env", "id")
		s, err := r.Manager.Open(id, stringOpt(opts, "platform"), stringOpt(opts, "url"), stringOpt(opts, "proxy"), boolOpt(opts, "headless"), stringOpt(opts, "chromeVersion", "chrome_version"))
		if err != nil {
			panic(vm.NewGoError(err))
		}
		r.track(s.ID)
		return r.pageObject(vm, s)
	})

	_ = ac.Set("session", func(call goja.FunctionCall) goja.Value {
		id := call.Argument(0).String()
		s, err := r.Manager.Get(id)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return r.pageObject(vm, s)
	})

	_ = ac.Set("log", func(call goja.FunctionCall) goja.Value {
		r.log(call)
		return goja.Undefined()
	})

	_ = ac.Set("user", r.User.ID)

	env := vm.NewObject()
	_ = env.Set("create", func(call goja.FunctionCall) goja.Value {
		opts := objectArg(call, 0)
		id := stringOpt(opts, "id")
		if id == "" {
			id = stringOpt(opts, "env")
		}
		platform := stringOpt(opts, "platform")
		if id == "" {
			panic(vm.NewGoError(fmt.Errorf("id is required")))
		}
		if m, ok := r.Registry.Get(id); ok {
			if m.Dir != "" {
				if _, err := os.Stat(m.Dir); err == nil {
					panic(vm.NewGoError(fmt.Errorf("environment %q already exists", id)))
				}
			}
			_ = r.Registry.Delete(id)
		}
		if _, _, ok := envdir.Find(r.User.EnvRoot, id); ok {
			panic(vm.NewGoError(fmt.Errorf("environment %q already exists", id)))
		}
		if r.Registry.NameTaken(id, "") {
			panic(vm.NewGoError(fmt.Errorf("container name %q already exists", id)))
		}
		fp := fingerprint.Generate(id)
		dir, err := envdir.Create(r.User.EnvRoot, platform, id, fp)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		if meta, ok := envdir.ReadMeta(dir); ok {
			if cv := stringOpt(opts, "chromeVersion", "chrome_version"); cv != "" {
				if updated, err := envdir.SetChromeDir(dir, meta, cv); err == nil {
					meta = updated
				}
			}
			_ = r.Registry.Put(meta)
		}
		return vm.ToValue(map[string]any{"dir": dir, "fingerprint": fp})
	})
	_ = env.Set("list", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(r.listEnvs())
	})
	_ = ac.Set("env", env)

	return ac
}

func (r *Runner) listEnvs() []envdir.Meta {
	byID := map[string]envdir.Meta{}
	if list, err := r.Registry.List(); err == nil {
		for _, m := range list {
			dir := m.Dir
			if dir == "" {
				dir = filepath.Join(r.User.EnvRoot, envdir.Name(m.Platform, m.ID))
			}
			if _, err := os.Stat(dir); err != nil {
				_ = r.Registry.Delete(m.ID)
				continue
			}
			byID[m.ID] = m
		}
	}
	if list, err := envdir.List(r.User.EnvRoot); err == nil {
		for _, m := range list {
			if _, ok := byID[m.ID]; ok {
				continue
			}
			byID[m.ID] = m
			_ = r.Registry.Put(m)
		}
	}
	out := make([]envdir.Meta, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Runner) pageObject(vm *goja.Runtime, s *session.Session) *goja.Object {
	page := vm.NewObject()
	_ = page.Set("id", s.ID)
	_ = page.Set("env", s.EnvID)

	_ = page.Set("goto", func(call goja.FunctionCall) goja.Value {
		url := call.Argument(0).String()
		if err := s.Do(func(ctx context.Context) error { return browser.Navigate(ctx, url) }); err != nil {
			panic(vm.NewGoError(err))
		}
		s.SetURL(url)
		return goja.Undefined()
	})

	_ = page.Set("reload", func(call goja.FunctionCall) goja.Value {
		if err := s.Do(func(ctx context.Context) error { return browser.Reload(ctx) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("back", func(call goja.FunctionCall) goja.Value {
		if err := s.Do(func(ctx context.Context) error { return browser.Back(ctx) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("url", func(call goja.FunctionCall) goja.Value {
		var out string
		err := s.Do(func(ctx context.Context) error {
			v, e := browser.Location(ctx)
			out = v
			return e
		})
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(out)
	})

	_ = page.Set("title", func(call goja.FunctionCall) goja.Value {
		var out string
		err := s.Do(func(ctx context.Context) error {
			v, e := browser.Title(ctx)
			out = v
			return e
		})
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(out)
	})

	_ = page.Set("snapshot", func(call goja.FunctionCall) goja.Value {
		opts := objectArg(call, 0)
		max := intOpt(opts, "max")
		var snap browser.Snapshot
		err := s.Do(func(ctx context.Context) error {
			v, e := browser.TakeSnapshot(ctx, max)
			snap = v
			return e
		})
		if err != nil {
			panic(vm.NewGoError(err))
		}
		s.SetURL(snap.URL)
		return vm.ToValue(snap)
	})

	_ = page.Set("click", func(call goja.FunctionCall) goja.Value {
		target := targetFrom(call.Argument(0))
		if err := s.Do(func(ctx context.Context) error { return browser.Click(ctx, target) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("type", func(call goja.FunctionCall) goja.Value {
		target := targetFrom(call.Argument(0))
		text := call.Argument(1).String()
		if err := s.Do(func(ctx context.Context) error { return browser.Type(ctx, target, text) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("press", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		if err := s.Do(func(ctx context.Context) error { return browser.Press(ctx, key) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("wait", func(call goja.FunctionCall) goja.Value {
		opts := objectArg(call, 0)
		timeout := time.Duration(intOpt(opts, "timeout")) * time.Millisecond
		waitOpts := browser.WaitOptions{
			Selector:       stringOpt(opts, "selector"),
			HiddenSelector: stringOpt(opts, "hiddenSelector", "hidden_selector"),
			Text:           stringOpt(opts, "text"),
			URLContains:    stringOpt(opts, "urlContains", "url_contains"),
			CookieName:     stringOpt(opts, "cookie", "cookieName", "cookie_name"),
			Timeout:        timeout,
		}
		if err := s.Do(func(ctx context.Context) error {
			return browser.WaitForOptions(ctx, waitOpts)
		}); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("eval", func(call goja.FunctionCall) goja.Value {
		if !r.AllowEval {
			panic(vm.NewGoError(fmt.Errorf("eval is disabled for user %q", r.User.ID)))
		}
		expr := call.Argument(0).String()
		var out any
		err := s.Do(func(ctx context.Context) error {
			v, e := browser.Eval(ctx, expr)
			out = v
			return e
		})
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(out)
	})

	_ = page.Set("screenshot", func(call goja.FunctionCall) goja.Value {
		opts := objectArg(call, 0)
		path := stringOpt(opts, "path")
		fullPage := boolOpt(opts, "fullPage")
		var buf []byte
		err := s.Do(func(ctx context.Context) error {
			b, e := browser.CapturePNG(ctx, fullPage)
			buf = b
			return e
		})
		if err != nil {
			panic(vm.NewGoError(err))
		}
		res := map[string]any{"bytes": len(buf)}
		if path != "" {
			if !filepath.IsAbs(path) {
				path = filepath.Join(r.User.ShotRoot, path)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				panic(vm.NewGoError(err))
			}
			if err := os.WriteFile(path, buf, 0o600); err != nil {
				panic(vm.NewGoError(err))
			}
			res["path"] = path
		}
		if path == "" || boolOpt(opts, "inline") {
			res["base64"] = base64.StdEncoding.EncodeToString(buf)
		}
		return vm.ToValue(res)
	})

	_ = page.Set("cookies", func(call goja.FunctionCall) goja.Value {
		opts := objectArg(call, 0)
		var urls []string
		if raw, ok := opts["urls"]; ok {
			switch list := raw.(type) {
			case []string:
				urls = append(urls, list...)
			case []any:
				for _, v := range list {
					if item, ok := v.(string); ok {
						urls = append(urls, item)
					}
				}
			}
		}
		var cookies any
		err := s.Do(func(ctx context.Context) error {
			c, e := browser.GetCookies(ctx, urls)
			cookies = c
			return e
		})
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(cookies)
	})

	_ = page.Set("detectChallenge", func(call goja.FunctionCall) goja.Value {
		var out []browser.ChallengeCandidate
		if err := s.Do(func(ctx context.Context) error {
			v, e := browser.DetectChallenges(ctx)
			out = v
			return e
		}); err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(out)
	})

	_ = page.Set("qr", func(call goja.FunctionCall) goja.Value {
		var buf []byte
		var cand browser.ChallengeCandidate
		if err := s.Do(func(ctx context.Context) error {
			b, c, e := browser.QRCodePNG(ctx)
			buf, cand = b, c
			return e
		}); err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(map[string]any{
			"selector": cand.Selector,
			"name":     cand.Name,
			"base64":   base64.StdEncoding.EncodeToString(buf),
		})
	})

	_ = page.Set("clickXY", func(call goja.FunctionCall) goja.Value {
		x := call.Argument(0).ToFloat()
		y := call.Argument(1).ToFloat()
		if err := s.Do(func(ctx context.Context) error { return browser.ClickXY(ctx, x, y) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("scroll", func(call goja.FunctionCall) goja.Value {
		dx := int(call.Argument(0).ToInteger())
		dy := int(call.Argument(1).ToInteger())
		if err := s.Do(func(ctx context.Context) error { return browser.Scroll(ctx, dx, dy) }); err != nil {
			panic(vm.NewGoError(err))
		}
		return goja.Undefined()
	})

	_ = page.Set("close", func(call goja.FunctionCall) goja.Value {
		_ = r.Manager.Close(s.ID)
		return goja.Undefined()
	})

	return page
}

func (r *Runner) track(id string) {
	if r.openMarker == nil {
		r.openMarker = map[string]bool{}
	}
	if r.openMarker[id] {
		return
	}
	r.openMarker[id] = true
	r.opened = append(r.opened, id)
}

func objectArg(call goja.FunctionCall, i int) map[string]any {
	v := call.Argument(i)
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return map[string]any{}
	}
	m, ok := v.Export().(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

func stringOpt(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func intOpt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

func boolOpt(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func targetFrom(v goja.Value) browser.Target {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return browser.Target{}
	}
	if obj, ok := v.(*goja.Object); ok {
		var t browser.Target
		if x := obj.Get("ref"); x != nil && !goja.IsUndefined(x) && !goja.IsNull(x) {
			t.Ref = x.String()
		}
		if x := obj.Get("selector"); x != nil && !goja.IsUndefined(x) && !goja.IsNull(x) {
			t.Selector = x.String()
		}
		if x := obj.Get("text"); x != nil && !goja.IsUndefined(x) && !goja.IsNull(x) {
			t.Text = x.String()
		}
		return t
	}
	s := v.String()
	if refPattern.MatchString(s) {
		return browser.Target{Ref: s}
	}
	return browser.Target{Selector: s}
}
