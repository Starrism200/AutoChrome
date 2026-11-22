// Package session owns long-lived browser sessions for the MCP server and the
// script runner. Each session keeps one Chrome process and profile alive
// across many tool calls.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"autochrome/internal/browser"
	chromepkg "autochrome/internal/chrome"
	"autochrome/internal/envdir"
	"autochrome/internal/fingerprint"
	"autochrome/internal/users"
	"autochrome/internal/xvfb"
)

// Info is a serializable session summary.
type Info struct {
	ID            string `json:"id"`
	EnvID         string `json:"env_id"`
	Platform      string `json:"platform"`
	URL           string `json:"url,omitempty"`
	Proxy         string `json:"proxy,omitempty"`
	ChromeVersion string `json:"chrome_version,omitempty"`
	Headless      bool   `json:"headless"`
	CreatedAt     string `json:"created_at"`
}

// Session is one live Chrome process.
type Session struct {
	ID            string
	EnvID         string
	Platform      string
	ProfileDir    string
	Proxy         string
	ChromeVersion string
	Headless      bool
	CreatedAt     time.Time

	manager *Manager
	ctx     context.Context
	cleanup func()
	once    sync.Once
	mu      sync.Mutex
	lastURL string
}

// Manager creates, stores and closes sessions for one user.
type Manager struct {
	user       users.User
	chromePath string
	chromeMgr  *chromepkg.Manager
	registry   *envdir.Registry

	mu              sync.Mutex
	sessions        map[string]*Session
	opening         map[string]bool
	display         *xvfb.Display
	externalDisplay *xvfb.Display
}

// NewManager returns a manager bound to one user.
func NewManager(user users.User, chrome string) *Manager {
	if chrome == "" {
		chrome = "/usr/bin/google-chrome"
	}
	return &Manager{
		user:       user,
		chromePath: chrome,
		chromeMgr:  chromepkg.NewManager(),
		registry:   envdir.NewRegistry(user.RegistryPath()),
		sessions:   map[string]*Session{},
		opening:    map[string]bool{},
	}
}

// User returns the owner of this manager.
func (m *Manager) User() users.User {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.user
}

// SetUser updates the manager after user-level storage settings change.
func (m *Manager) SetUser(u users.User) {
	m.mu.Lock()
	m.user = u
	m.mu.Unlock()
}

func (m *Manager) userSnapshot() users.User {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.user
}

// SetDisplay makes the manager use a display owned by someone else (for
// example the desktop/VNC service) instead of starting its own Xvfb.
func (m *Manager) SetDisplay(d *xvfb.Display) {
	m.mu.Lock()
	m.externalDisplay = d
	m.mu.Unlock()
}

// Display starts (once) and returns the shared virtual display.
func (m *Manager) Display() (*xvfb.Display, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.externalDisplay != nil {
		return m.externalDisplay, nil
	}
	if m.display != nil {
		return m.display, nil
	}
	d, err := xvfb.Start("1366x900x24")
	if err != nil {
		return nil, err
	}
	m.display = d
	return d, nil
}

// Open launches a browser for envID and returns a stored session.
func (m *Manager) Open(envID, platform, url, proxy string, headless bool, chromeVersion string) (*Session, error) {
	if envID == "" {
		return nil, fmt.Errorf("env id is required")
	}
	if platform == "" {
		platform = "default"
	}
	m.mu.Lock()
	for _, existing := range m.sessions {
		if existing.EnvID == envID {
			id := existing.ID
			m.mu.Unlock()
			return nil, fmt.Errorf("environment %q is already open (session %s)", envID, id)
		}
	}
	if m.opening[envID] {
		m.mu.Unlock()
		return nil, fmt.Errorf("environment %q is already opening", envID)
	}
	m.opening[envID] = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.opening, envID)
		m.mu.Unlock()
	}()

	user := m.userSnapshot()
	var dir string
	var meta envdir.Meta
	hasMeta := false
	if reg, ok := m.registry.Get(envID); ok && reg.Dir != "" {
		dir, meta, hasMeta = reg.Dir, reg, true
	} else {
		dir = filepath.Join(user.EnvRoot, envdir.Name(platform, envID))
	}
	if stored, ok := envdir.ReadMeta(dir); ok {
		if stored.ID != "" && stored.ID != envID {
			return nil, fmt.Errorf("environment directory %q contains environment %q", dir, stored.ID)
		}
		meta, hasMeta = stored, true
	}
	if hasMeta && meta.ID != "" && meta.ID != envID {
		return nil, fmt.Errorf("environment directory %q contains environment %q", dir, meta.ID)
	}
	fp := fingerprint.Generate(envID)
	if hasMeta && meta.Fingerprint.UserAgent != "" {
		fp = meta.Fingerprint
	}
	if _, ok := envdir.ReadMeta(dir); !ok {
		created, err := envdir.CreateAt(dir, platform, envID, fp)
		if err != nil {
			return nil, err
		}
		if hasMeta {
			created.ProxyID, created.Proxy = meta.ProxyID, meta.Proxy
			created.ChromeVersion = meta.ChromeVersion
			_, _ = envdir.SetProxyDir(dir, created, created.ProxyID, created.Proxy)
		}
		meta, hasMeta = created, true
	}
	meta.ID, meta.Dir = envID, dir
	if meta.Platform == "" {
		meta.Platform = platform
	}
	_ = m.registry.Put(meta)
	if proxy == "" && hasMeta {
		proxy = meta.Proxy
	}
	if proxy == "" {
		proxy = user.DefaultProxy
	}
	version := chromeVersion
	if version == "" {
		version = meta.ChromeVersion
	}
	if version == "" {
		version = user.ChromeVersion
	}
	chromePath, err := m.chromeMgr.Resolve(version, m.chromePath)
	if err != nil {
		return nil, err
	}
	displayVersion := version
	if displayVersion == "" {
		if cur, err := m.chromeMgr.Current(); err == nil {
			displayVersion = cur.Version
		}
	}

	var env []string
	if !headless {
		d, err := m.Display()
		if err != nil {
			return nil, err
		}
		env = d.Env()
	}

	ctx, cleanup, err := browser.Launch(browser.Options{
		ChromePath:  chromePath,
		ProfileDir:  dir,
		ProxyAddr:   proxy,
		Headless:    headless,
		Timeout:     -1,
		Fingerprint: &fp,
		SlotDir:     user.SlotDir,
		SlotMax:     user.BrowserMax(),
		Env:         env,
	})
	if err != nil {
		return nil, err
	}

	s := &Session{
		ID:            newID(),
		EnvID:         envID,
		Platform:      platform,
		ProfileDir:    dir,
		Proxy:         proxy,
		ChromeVersion: displayVersion,
		Headless:      headless,
		CreatedAt:     time.Now(),
		manager:       m,
		ctx:           ctx,
		cleanup:       cleanup,
	}

	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()

	if url != "" {
		if err := s.Do(func(ctx context.Context) error {
			return browser.Navigate(ctx, url)
		}); err != nil {
			_ = m.Close(s.ID)
			return nil, err
		}
		s.SetURL(url)
	}
	return s, nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}
	return s, nil
}

// Close closes one session.
func (m *Manager) Close(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("session %q not found", id)
	}
	s.close()
	return nil
}

// CloseAll closes every session and the shared display.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	for _, s := range sessions {
		s.close()
	}
	m.mu.Lock()
	if m.display != nil {
		_ = m.display.Close()
		m.display = nil
	}
	m.mu.Unlock()
}

// SessionForEnv returns the live session for envID, if any.
func (m *Manager) SessionForEnv(envID string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.EnvID == envID {
			return s, true
		}
	}
	return nil, false
}

// CloseEnv closes every session opened for envID.
func (m *Manager) CloseEnv(envID string) {
	m.mu.Lock()
	ids := []string{}
	for id, s := range m.sessions {
		if s.EnvID == envID {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Close(id)
	}
}

// List returns session summaries sorted by creation time.
func (m *Manager) List() []Info {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt.Before(sessions[j].CreatedAt) })
	out := make([]Info, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Info())
	}
	return out
}

// Do runs fn while holding the session lock. Chrome/CDP actions on one tab are
// serialized so concurrent tool calls cannot interleave.
func (s *Session) Do(fn func(context.Context) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s.ctx)
}

// Info returns a summary of the session.
func (s *Session) Info() Info {
	s.mu.Lock()
	url := s.lastURL
	s.mu.Unlock()
	return Info{
		ID:            s.ID,
		EnvID:         s.EnvID,
		Platform:      s.Platform,
		URL:           url,
		Proxy:         s.Proxy,
		ChromeVersion: s.ChromeVersion,
		Headless:      s.Headless,
		CreatedAt:     s.CreatedAt.Format(time.RFC3339),
	}
}

// SetURL records the last known URL.
func (s *Session) SetURL(url string) {
	s.mu.Lock()
	s.lastURL = url
	s.mu.Unlock()
}

func (s *Session) close() {
	s.once.Do(func() {
		if s.cleanup != nil {
			s.cleanup()
		}
	})
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("s%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
