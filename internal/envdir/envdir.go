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

	"autochrome/internal/atomicfile"
	"autochrome/internal/fingerprint"
)

const MetaFile = "env.json"

type Meta struct {
	ID            string              `json:"id"`
	Name          string              `json:"name,omitempty"`
	Description   string              `json:"description,omitempty"`
	Tags          []string            `json:"tags,omitempty"`
	Platform      string              `json:"platform"`
	Dir           string              `json:"dir"`
	CreatedAt     string              `json:"created_at"`
	UpdatedAt     string              `json:"updated_at,omitempty"`
	ProxyID       string              `json:"proxy_id,omitempty"`
	Proxy         string              `json:"proxy,omitempty"`
	ChromeVersion string              `json:"chrome_version,omitempty"`
	DownloadDir   string              `json:"download_dir,omitempty"`
	ScreenshotDir string              `json:"screenshot_dir,omitempty"`
	AttachmentDir string              `json:"attachment_dir,omitempty"`
	Fingerprint   fingerprint.Profile `json:"fingerprint"`
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
func (m *Meta) FillPaths() {
	if m.Dir == "" {
		return
	}
	if m.DownloadDir == "" {
		m.DownloadDir = filepath.Join(m.Dir, "downloads")
	}
	if m.ScreenshotDir == "" {
		m.ScreenshotDir = filepath.Join(m.Dir, "screenshots")
	}
	if m.AttachmentDir == "" {
		m.AttachmentDir = filepath.Join(m.Dir, "attachments")
	}
}

func CreateAt(dir, platform, id string, fp fingerprint.Profile) (Meta, error) {
	if strings.TrimSpace(id) == "" {
		return Meta{}, fmt.Errorf("empty environment id")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Meta{}, err
	}
	if meta, ok := ReadMeta(dir); ok {
		if meta.ID != "" && meta.ID != id {
			return Meta{}, fmt.Errorf("directory %q contains environment %q", dir, meta.ID)
		}
		return meta, nil
	}
	meta := Meta{ID: id, Platform: platform, Dir: dir, CreatedAt: time.Now().Format(time.RFC3339), Fingerprint: fp}
	meta.FillPaths()
	for _, sub := range []string{meta.DownloadDir, meta.ScreenshotDir, meta.AttachmentDir} {
		if err := os.MkdirAll(sub, 0o755); err != nil {
			return Meta{}, err
		}
	}
	if err := writeMeta(dir, meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// WriteMeta writes an environment metadata file.
func WriteMeta(dir string, meta Meta) error { return writeMeta(dir, meta) }

func writeMeta(dir string, meta Meta) error {
	meta.Dir = dir
	meta.FillPaths()
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, MetaFile), b, 0o600)
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
	m.FillPaths()
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

// SetSizeDir updates the per-environment browser viewport size.
func SetSizeDir(dir string, meta Meta, width, height int) (Meta, error) {
	meta.Fingerprint.Width = width
	meta.Fingerprint.Height = height
	meta.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := writeMeta(dir, meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// SetFingerprintDir updates the entire fingerprint profile for an environment.
func SetFingerprintDir(dir string, meta Meta, fp fingerprint.Profile) (Meta, error) {
	meta.Fingerprint = fp
	meta.UpdatedAt = time.Now().Format(time.RFC3339)
	if err := writeMeta(dir, meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// SetChromeDir writes a Chrome version binding to an explicit environment.
func SetChromeDir(dir string, meta Meta, version string) (Meta, error) {
	meta.ChromeVersion = version
	if err := writeMeta(dir, meta); err != nil {
		return Meta{}, err
	}
	return meta, nil
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
