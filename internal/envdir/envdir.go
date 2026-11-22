// Package envdir manages per-account browser environment directories.
//
// Every account owns one directory. The directory is self-contained so it can
// be archived, copied and restored independently. The root can change over
// time; existing accounts keep the absolute directory recorded at creation.
package envdir

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"autochrome/internal/fingerprint"
)

const MetaFile = "env.json"

type Meta struct {
	ID          string              `json:"id"`
	Platform    string              `json:"platform"`
	Dir         string              `json:"dir"`
	CreatedAt   string              `json:"created_at"`
	ProxyID     string              `json:"proxy_id,omitempty"`
	Proxy       string              `json:"proxy,omitempty"`
	Fingerprint fingerprint.Profile `json:"fingerprint"`
}

var unsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// Sanitize keeps a directory name portable.
func Sanitize(s string) string {
	s = strings.TrimSpace(s)
	s = unsafe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._-")
	if s == "" {
		return "env"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// Root returns the active environment root. AUTOCHROME_ENV_ROOT wins, then the
// supplied fallback (usually USER_DIR).
func Root(fallback string) string {
	if v := strings.TrimSpace(os.Getenv("AUTOCHROME_ENV_ROOT")); v != "" {
		return v
	}
	return fallback
}

// Name returns the default directory name for an account.
func Name(platform, id string) string {
	p := strings.ToLower(strings.TrimSpace(platform))
	if p == "" {
		p = "default"
	}
	return p + "-" + Sanitize(id)
}

// Create makes the environment directory under root and writes its metadata.
func Create(root, platform, id string, fp fingerprint.Profile) (string, error) {
	dir := filepath.Join(root, Name(platform, id))
	if _, err := CreateAt(dir, platform, id, fp); err != nil {
		return "", err
	}
	return dir, nil
}

// CreateAt makes an environment at an explicit directory.
func CreateAt(dir, platform, id string, fp fingerprint.Profile) (Meta, error) {
	if strings.TrimSpace(id) == "" {
		return Meta{}, fmt.Errorf("empty environment id")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Meta{}, err
	}
	if meta, ok := ReadMeta(dir); ok {
		return meta, nil
	}
	meta := Meta{ID: id, Platform: platform, Dir: dir, CreatedAt: time.Now().Format(time.RFC3339), Fingerprint: fp}
	if err := writeMeta(dir, meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

func writeMeta(dir string, meta Meta) error {
	meta.Dir = dir
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, MetaFile), b, 0o600)
}

// Ensure makes sure an existing environment directory has metadata.
func Ensure(dir string, meta Meta) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, MetaFile)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return writeMeta(dir, meta)
}

// ReadMeta loads env.json if present.
func ReadMeta(dir string) (Meta, bool) {
	b, err := os.ReadFile(filepath.Join(dir, MetaFile))
	if err != nil {
		return Meta{}, false
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return Meta{}, false
	}
	return m, true
}

// List returns every environment under root, sorted by id.
func List(root string) ([]Meta, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Meta{}, nil
		}
		return nil, err
	}
	out := []Meta{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if meta, ok := ReadMeta(filepath.Join(root, e.Name())); ok {
			out = append(out, meta)
		}
	}
	return out, nil
}

// Find locates an environment by id and returns its metadata and directory.
func Find(root, id string) (Meta, string, bool) {
	list, _ := List(root)
	for _, meta := range list {
		if meta.ID == id {
			dir := meta.Dir
			if dir == "" {
				dir = filepath.Join(root, Name(meta.Platform, meta.ID))
			}
			return meta, dir, true
		}
	}
	return Meta{}, "", false
}

// SetProxy binds a proxy id/url to an environment.
func SetProxy(root, id, proxyID, proxyURL string) (Meta, error) {
	meta, dir, ok := Find(root, id)
	if !ok {
		return Meta{}, fmt.Errorf("environment %q not found", id)
	}
	if !withinRoot(root, dir) {
		return Meta{}, fmt.Errorf("refusing to write %q outside %q", dir, root)
	}
	return SetProxyDir(dir, meta, proxyID, proxyURL)
}

// SetProxyDir writes a proxy binding to an explicit environment directory.
func SetProxyDir(dir string, meta Meta, proxyID, proxyURL string) (Meta, error) {
	meta.ProxyID = proxyID
	meta.Proxy = proxyURL
	if err := writeMeta(dir, meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// RemoveDir deletes an environment directory recorded in the registry.
func RemoveDir(dir string) error {
	clean := filepath.Clean(dir)
	if clean == "" || clean == "." || clean == string(os.PathSeparator) {
		return fmt.Errorf("refusing to remove %q", dir)
	}
	return os.RemoveAll(clean)
}

// Delete removes an environment directory.
func Delete(root, id string) error {
	_, dir, ok := Find(root, id)
	if !ok {
		return fmt.Errorf("environment %q not found", id)
	}
	if !withinRoot(root, dir) {
		return fmt.Errorf("refusing to delete %q outside %q", dir, root)
	}
	return os.RemoveAll(dir)
}

func withinRoot(root, dir string) bool {
	cleanRoot := filepath.Clean(root)
	cleanDir := filepath.Clean(dir)
	if cleanDir == cleanRoot {
		return false
	}
	return strings.HasPrefix(cleanDir+string(os.PathSeparator), cleanRoot+string(os.PathSeparator))
}
