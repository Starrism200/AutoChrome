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

// Create makes the environment directory and writes its metadata file.
func Create(root, platform, id string, fp fingerprint.Profile) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("empty environment id")
	}
	dir := filepath.Join(root, Name(platform, id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	meta := Meta{ID: id, Platform: platform, Dir: dir, CreatedAt: time.Now().Format(time.RFC3339), Fingerprint: fp}
	if _, err := os.Stat(filepath.Join(dir, MetaFile)); err != nil {
		b, _ := json.MarshalIndent(meta, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, MetaFile), b, 0o644); err != nil {
			return "", err
		}
	}
	return dir, nil
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
	meta.Dir = dir
	b, _ := json.MarshalIndent(meta, "", "  ")
	return os.WriteFile(p, b, 0o644)
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
