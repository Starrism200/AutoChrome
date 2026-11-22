// Package users resolves per-user directories and settings.
//
// Each user owns a self-contained tree. The default user keeps the historical
// project layout (profiles/, scripts/custom/, data/); every other user lives
// under data/users/<id>/ so a separate MCP process can be pointed at it.
package users

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"autochrome/internal/atomicfile"
	"autochrome/internal/filelock"
)

const DefaultID = "default"

const (
	DefaultVNCPortStart = 10000
	DefaultVNCPortEnd   = 59999
)

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// User is one isolated environment owner.
type User struct {
	ID                 string   `json:"id"`
	CreatedAt          string   `json:"created_at,omitempty"`
	MaxEnvs            int      `json:"max_envs,omitempty"`
	MaxBrowsers        int      `json:"max_browsers,omitempty"`
	AllowEval          bool     `json:"allow_eval"`
	AllowScripts       bool     `json:"allow_scripts"`
	AllowedHosts       []string `json:"allowed_hosts,omitempty"`
	DefaultProxy       string   `json:"default_proxy,omitempty"`
	VNCPortStart       int      `json:"vnc_port_start,omitempty"`
	VNCPortEnd         int      `json:"vnc_port_end,omitempty"`
	EnvRootPath        string   `json:"env_root,omitempty"`
	AttachmentRootPath string   `json:"attachment_root,omitempty"`
	ChromeVersion      string   `json:"chrome_version,omitempty"`

	Home           string `json:"-"`
	Root           string `json:"-"`
	EnvRoot        string `json:"-"`
	AttachmentRoot string `json:"-"`
	ScriptRoot     string `json:"-"`
	ShotRoot       string `json:"-"`
	LogRoot        string `json:"-"`
	SlotDir        string `json:"-"`
}

// Home returns the project root. AUTOCHROME_HOME wins, then the parent of the
// directory holding the executable, then the working directory.
func Home() string {
	if v := strings.TrimSpace(os.Getenv("AUTOCHROME_HOME")); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			return abs
		}
		return v
	}
	if exe, err := os.Executable(); err == nil {
		root := filepath.Dir(filepath.Dir(exe))
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			return root
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// Resolve loads a user by id. An empty id falls back to AUTOCHROME_USER and
// then to "default".
func Resolve(id string) (User, error) {
	if strings.TrimSpace(id) == "" {
		id = strings.TrimSpace(os.Getenv("AUTOCHROME_USER"))
	}
	if id == "" {
		id = DefaultID
	}
	if !validID.MatchString(id) {
		return User{}, fmt.Errorf("invalid user id %q (lowercase letters, digits, - and _)", id)
	}

	home := Home()
	u := User{ID: id, Home: home}
	if id == DefaultID {
		u.Root = home
		u.EnvRoot = filepath.Join(home, "profiles")
		u.AttachmentRoot = filepath.Join(home, "data", "attachments")
		u.ScriptRoot = filepath.Join(home, "scripts", "custom")
		u.ShotRoot = filepath.Join(home, "data", "screenshots")
		u.LogRoot = filepath.Join(home, "data", "logs")
		u.SlotDir = filepath.Join(home, "data", "browser-slots")
		u.MaxBrowsers = 10
		u.AllowEval = true
		u.AllowScripts = true
		u.VNCPortStart = DefaultVNCPortStart
		u.VNCPortEnd = DefaultVNCPortEnd
		_ = u.load(u.settingsPath())
		return u, nil
	}

	u.Root = filepath.Join(home, "data", "users", id)
	u.EnvRoot = filepath.Join(u.Root, "profiles")
	u.AttachmentRoot = filepath.Join(u.Root, "attachments")
	u.ScriptRoot = filepath.Join(u.Root, "scripts")
	u.ShotRoot = filepath.Join(u.Root, "screenshots")
	u.LogRoot = filepath.Join(u.Root, "logs")
	u.SlotDir = filepath.Join(u.Root, "browser-slots")
	u.VNCPortStart = DefaultVNCPortStart
	u.VNCPortEnd = DefaultVNCPortEnd
	if err := u.load(u.settingsPath()); err != nil {
		if os.IsNotExist(err) {
			return User{}, fmt.Errorf("user %q not found; run: autochrome user add %s", id, id)
		}
		return User{}, err
	}
	return u, nil
}

// Add creates a user tree and settings file. The default user is always
// available and is returned as-is.
func Add(id string) (User, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return User{}, fmt.Errorf("user id is required")
	}
	if id == DefaultID {
		u, err := Resolve(id)
		if err != nil {
			return User{}, err
		}
		u.AllowEval = true
		u.AllowScripts = true
		return u, u.Save()
	}
	if !validID.MatchString(id) {
		return User{}, fmt.Errorf("invalid user id %q (lowercase letters, digits, - and _)", id)
	}
	home := Home()
	root := filepath.Join(home, "data", "users", id)
	u := User{ID: id, Home: home, Root: root}
	u.EnvRoot = filepath.Join(root, "profiles")
	u.AttachmentRoot = filepath.Join(root, "attachments")
	u.ScriptRoot = filepath.Join(root, "scripts")
	u.ShotRoot = filepath.Join(root, "screenshots")
	u.LogRoot = filepath.Join(root, "logs")
	u.SlotDir = filepath.Join(root, "browser-slots")
	if existing, err := Resolve(id); err == nil {
		u = existing
	} else {
		u.CreatedAt = time.Now().Format(time.RFC3339)
		u.MaxEnvs = 10
		u.MaxBrowsers = 4
		u.VNCPortStart = DefaultVNCPortStart
		u.VNCPortEnd = DefaultVNCPortEnd
	}
	if err := u.EnsureDirs(); err != nil {
		return User{}, err
	}
	if err := u.Save(); err != nil {
		return User{}, err
	}
	return u, nil
}

// List returns the default user and every registered user, sorted by id.
func List() ([]User, error) {
	out := []User{}
	if u, err := Resolve(DefaultID); err == nil {
		out = append(out, u)
	}
	dir := filepath.Join(Home(), "data", "users")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if u, err := Resolve(e.Name()); err == nil {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Save writes the user settings file.
func (u User) Save() error {
	if err := u.EnsureDirs(); err != nil {
		return err
	}
	path := u.settingsPath()
	lock, err := filelock.Acquire(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(path, b, 0o600)
}

// EnsureDirs creates every directory owned by the user.
func (u User) EnsureDirs() error {
	for _, dir := range []string{u.EnvRoot, u.AttachmentRoot, u.ScriptRoot, u.ShotRoot, u.LogRoot, u.SlotDir} {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// ApplyPaths resolves user-level storage overrides.
func (u *User) applyPaths() {
	if u.EnvRootPath != "" {
		u.EnvRoot = absPath(u.Home, u.EnvRootPath)
	}
	if u.AttachmentRootPath != "" {
		u.AttachmentRoot = absPath(u.Home, u.AttachmentRootPath)
	}
}

// SetStorage persists user-level environment and attachment roots.
func (u *User) SetStorage(envRoot, attachmentRoot string) error {
	newEnvPath, newEnv := u.EnvRootPath, u.EnvRoot
	newAttPath, newAtt := u.AttachmentRootPath, u.AttachmentRoot
	if envRoot != "" {
		p := absPath(u.Home, envRoot)
		if !validRoot(p) {
			return fmt.Errorf("invalid env_root %q", envRoot)
		}
		newEnvPath, newEnv = p, p
	}
	if attachmentRoot != "" {
		p := absPath(u.Home, attachmentRoot)
		if !validRoot(p) {
			return fmt.Errorf("invalid attachment_root %q", attachmentRoot)
		}
		newAttPath, newAtt = p, p
	}
	oldEnvPath, oldEnv := u.EnvRootPath, u.EnvRoot
	oldAttPath, oldAtt := u.AttachmentRootPath, u.AttachmentRoot
	u.EnvRootPath, u.EnvRoot = newEnvPath, newEnv
	u.AttachmentRootPath, u.AttachmentRoot = newAttPath, newAtt
	if err := u.EnsureDirs(); err != nil {
		u.EnvRootPath, u.EnvRoot = oldEnvPath, oldEnv
		u.AttachmentRootPath, u.AttachmentRoot = oldAttPath, oldAtt
		return err
	}
	if err := u.Save(); err != nil {
		u.EnvRootPath, u.EnvRoot = oldEnvPath, oldEnv
		u.AttachmentRootPath, u.AttachmentRoot = oldAttPath, oldAtt
		return err
	}
	return nil
}

// RegistryPath is the per-user environment index.
func (u User) RegistryPath() string {
	return filepath.Join(u.Home, "data", "users", u.ID, "envs.json")
}

// AttachmentIndexPath is the per-user attachment index.
func (u User) AttachmentIndexPath() string {
	return filepath.Join(u.Home, "data", "users", u.ID, "attachments.json")
}

func validRoot(p string) bool {
	clean := filepath.Clean(p)
	return clean != "" && clean != "." && clean != string(os.PathSeparator)
}

func absPath(home, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(home, p)
}

// VNCRange returns the configured auto-allocation range for VNC ports.
func (u User) VNCRange() (int, int) {
	start, end := u.VNCPortStart, u.VNCPortEnd
	if start <= 0 {
		start = DefaultVNCPortStart
	}
	if end <= 0 {
		end = DefaultVNCPortEnd
	}
	if end < start {
		start, end = end, start
	}
	return start, end
}

// BrowserMax returns the effective browser cap.
func (u User) BrowserMax() int {
	if u.MaxBrowsers > 0 {
		return u.MaxBrowsers
	}
	return 10
}

func (u User) settingsPath() string {
	return filepath.Join(u.Home, "data", "users", u.ID, "user.json")
}

func (u *User) load(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var stored User
	if err := json.Unmarshal(b, &stored); err != nil {
		return err
	}
	if stored.ID == "" {
		stored.ID = u.ID
	}
	u.CreatedAt = stored.CreatedAt
	u.MaxEnvs = stored.MaxEnvs
	u.MaxBrowsers = stored.MaxBrowsers
	u.AllowEval = stored.AllowEval
	u.AllowScripts = stored.AllowScripts
	u.AllowedHosts = stored.AllowedHosts
	u.DefaultProxy = stored.DefaultProxy
	u.EnvRootPath = stored.EnvRootPath
	u.AttachmentRootPath = stored.AttachmentRootPath
	u.ChromeVersion = stored.ChromeVersion
	u.applyPaths()
	if stored.VNCPortStart > 0 {
		u.VNCPortStart = stored.VNCPortStart
	}
	if stored.VNCPortEnd > 0 {
		u.VNCPortEnd = stored.VNCPortEnd
	}
	return nil
}
