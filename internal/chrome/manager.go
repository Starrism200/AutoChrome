// Package chrome manages side-by-side Google Chrome versions.
package chrome

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"autochrome/internal/atomicfile"
)

const (
	// DefaultDir is the global managed Chrome directory.
	DefaultDir = "/opt/autochrome/chrome"
	// StableURL is the latest stable Debian package.
	StableURL = "https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb"
)

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Version is one installed Chrome build.
type Version struct {
	Version     string `json:"version"`
	Path        string `json:"path"`
	InstalledAt string `json:"installed_at,omitempty"`
	Managed     bool   `json:"managed"`
}

// Manager owns the managed Chrome versions.
type Manager struct {
	dir string
	mu  sync.Mutex
}

// NewManager returns a manager. AUTOCHROME_CHROME_DIR overrides DefaultDir.
func NewManager() *Manager {
	dir := strings.TrimSpace(os.Getenv("AUTOCHROME_CHROME_DIR"))
	if dir == "" {
		dir = DefaultDir
	}
	return &Manager{dir: dir}
}

// Dir returns the managed versions directory.
func (m *Manager) Dir() string { return m.dir }

// List returns all managed versions.
func (m *Manager) List() ([]Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.list()
}

func (m *Manager) list() ([]Version, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Version{}, nil
		}
		return nil, err
	}
	out := []Version{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		meta, err := readMeta(filepath.Join(m.dir, e.Name()))
		if err == nil {
			out = append(out, meta)
		}
	}
	return out, nil
}

// Get returns one managed version.
func (m *Manager) Get(version string) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.get(version)
}

func (m *Manager) get(version string) (Version, error) {
	path := filepath.Join(m.dir, safeVersion(version))
	meta, err := readMeta(path)
	if err == nil {
		return meta, nil
	}
	binary := filepath.Join(path, "opt", "google", "chrome", "chrome")
	if _, statErr := os.Stat(binary); statErr != nil {
		return Version{}, fmt.Errorf("chrome version %q not installed", version)
	}
	v, verr := VersionOf(binary)
	if verr != nil {
		v = version
	}
	meta = Version{Version: v, Path: binary, InstalledAt: time.Now().Format(time.RFC3339), Managed: true}
	_ = writeMeta(path, meta)
	return meta, nil
}

// Current returns the selected managed version, or the system Chrome.
func (m *Manager) Current() (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current()
}

func (m *Manager) current() (Version, error) {
	if b, err := os.ReadFile(filepath.Join(m.dir, "current")); err == nil {
		version := strings.TrimSpace(string(b))
		if version != "" {
			if v, err := m.get(version); err == nil {
				return v, nil
			}
		}
	}
	if _, err := os.Stat("/usr/bin/google-chrome"); err == nil {
		v, verr := VersionOf("/usr/bin/google-chrome")
		if verr != nil {
			v = "unknown"
		}
		return Version{Version: v, Path: "/usr/bin/google-chrome", Managed: false}, nil
	}
	return Version{}, fmt.Errorf("no Chrome available")
}

// Resolve returns the binary path for version. Empty, "default" and "system"
// fall back to the current managed build or the supplied default path.
func (m *Manager) Resolve(version, fallback string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "system" {
		if fallback != "" {
			return fallback, nil
		}
		return "/usr/bin/google-chrome", nil
	}
	if version == "" || version == "default" {
		cur, err := m.Current()
		if err == nil && cur.Path != "" {
			return cur.Path, nil
		}
		if fallback != "" {
			return fallback, nil
		}
		return "/usr/bin/google-chrome", nil
	}
	v, err := m.Get(version)
	if err != nil {
		return "", err
	}
	return v.Path, nil
}

// Use selects a managed version as current.
func (m *Manager) Use(version string) (Version, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.get(version)
	if err != nil {
		return Version{}, err
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return Version{}, err
	}
	if err := atomicfile.Write(filepath.Join(m.dir, "current"), []byte(v.Version+"\n"), 0o644); err != nil {
		return Version{}, err
	}
	return v, nil
}

// InstallDeb extracts a .deb into the managed directory.
func (m *Manager) InstallDeb(debPath string) (Version, error) {
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		return Version{}, fmt.Errorf("dpkg-deb not found")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return Version{}, err
	}
	tmp, err := os.MkdirTemp(m.dir, ".install-*")
	if err != nil {
		return Version{}, err
	}
	defer os.RemoveAll(tmp)
	cmd := exec.Command("dpkg-deb", "-x", debPath, tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Version{}, fmt.Errorf("dpkg-deb: %v: %s", err, strings.TrimSpace(string(out)))
	}
	binary := filepath.Join(tmp, "opt", "google", "chrome", "chrome")
	if _, err := os.Stat(binary); err != nil {
		return Version{}, fmt.Errorf("deb does not contain opt/google/chrome/chrome")
	}
	version, err := VersionOf(binary)
	if err != nil || version == "" {
		version = debVersion(debPath)
	}
	if version == "" {
		return Version{}, fmt.Errorf("could not determine Chrome version")
	}
	target := filepath.Join(m.dir, safeVersion(version))
	if existing, err := readMeta(target); err == nil {
		return existing, nil
	}
	if _, err := os.Stat(target); err == nil {
		return Version{}, fmt.Errorf("chrome version %q already exists", version)
	}
	_ = os.Chmod(binary, 0o755)
	if err := os.Rename(tmp, target); err != nil {
		return Version{}, err
	}
	meta := Version{
		Version: version, Path: filepath.Join(target, "opt", "google", "chrome", "chrome"),
		InstalledAt: time.Now().Format(time.RFC3339), Managed: true,
	}
	if err := writeMeta(target, meta); err != nil {
		return Version{}, err
	}
	return meta, nil
}

// InstallURL downloads a .deb and installs it.
func (m *Manager) InstallURL(rawURL string) (Version, error) {
	tmp, err := os.CreateTemp("", "autochrome-chrome-*.deb")
	if err != nil {
		return Version{}, err
	}
	path := tmp.Name()
	defer os.Remove(path)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(rawURL)
	if err != nil {
		tmp.Close()
		return Version{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return Version{}, fmt.Errorf("download failed: %s", resp.Status)
	}
	const maxDownload = int64(1 << 30)
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		tmp.Close()
		return Version{}, err
	}
	if n > maxDownload {
		tmp.Close()
		return Version{}, fmt.Errorf("download exceeds 1GB")
	}
	if err := tmp.Close(); err != nil {
		return Version{}, err
	}
	return m.InstallDeb(path)
}

// Update downloads the latest stable Chrome and selects it.
func (m *Manager) Update() (Version, error) {
	v, err := m.InstallURL(StableURL)
	if err != nil {
		return Version{}, err
	}
	return m.Use(v.Version)
}

// Remove deletes a managed version.
func (m *Manager) Remove(version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.get(version); err != nil {
		return err
	}
	if cur, err := m.current(); err == nil && cur.Version == version {
		return fmt.Errorf("cannot remove the current version")
	}
	return os.RemoveAll(filepath.Join(m.dir, safeVersion(version)))
}

// VersionOf runs `chrome --version` and returns the version string.
func VersionOf(binary string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, "--version").Output()
	if err != nil {
		return "", err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("empty version output")
	}
	return fields[len(fields)-1], nil
}

func debVersion(debPath string) string {
	out, err := exec.Command("dpkg-deb", "-f", debPath, "Version").Output()
	if err != nil {
		return ""
	}
	version := strings.TrimSpace(string(out))
	if i := strings.IndexByte(version, '-'); i > 0 {
		version = version[:i]
	}
	return version
}

func safeVersion(v string) string {
	v = unsafe.ReplaceAllString(strings.TrimSpace(v), "_")
	v = strings.Trim(v, "._-")
	if v == "" {
		return "unknown"
	}
	return v
}

func metaPath(dir string) string { return filepath.Join(dir, "chrome.json") }

func readMeta(dir string) (Version, error) {
	b, err := os.ReadFile(metaPath(dir))
	if err != nil {
		return Version{}, err
	}
	var v Version
	if err := json.Unmarshal(b, &v); err != nil {
		return Version{}, err
	}
	if v.Path == "" {
		return Version{}, fmt.Errorf("missing path")
	}
	if _, err := os.Stat(v.Path); err != nil {
		return Version{}, err
	}
	v.Managed = true
	return v, nil
}

func writeMeta(dir string, v Version) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(metaPath(dir), b, 0o644)
}
