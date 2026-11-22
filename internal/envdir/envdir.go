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
		p = "tiktok"
	}
	return p + "-" + Sanitize(id)
}

// LegacyName is the pre-migration TikTok naming scheme: NN-user.
func LegacyName(index int, user string) string {
	return fmt.Sprintf("%02d-%s", index, Sanitize(user))
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

// Resolve returns the directory an account should use. stored wins; otherwise
// the legacy directory is reused when it exists; otherwise a new directory
// name is returned (but not created).
func Resolve(stored, root, platform, id, legacy string) string {
	if strings.TrimSpace(stored) != "" {
		return stored
	}
	if legacy != "" {
		if st, err := os.Stat(legacy); err == nil && st.IsDir() {
			return legacy
		}
	}
	return filepath.Join(root, Name(platform, id))
}

type PanelEntry struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	Credentials string `json:"credentials"`
	Proxy       string `json:"proxy"`
	EnvDir      string `json:"env_dir"`
	Fingerprint string `json:"fingerprint"`
}

// LookupPanel finds a panel login entry by id, or by the username part of its
// credentials. It returns false when the snapshot is missing or has no match.
func LookupPanel(path, id, user string) (PanelEntry, bool) {
	if strings.TrimSpace(path) == "" {
		return PanelEntry{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return PanelEntry{}, false
	}
	var entries []PanelEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return PanelEntry{}, false
	}
	for _, e := range entries {
		if id != "" && e.ID == id {
			return e, true
		}
		if id == "" && user != "" && strings.TrimSpace(strings.SplitN(e.Credentials, "----", 2)[0]) == user {
			return e, true
		}
	}
	return PanelEntry{}, false
}

// Migration is one planned rename from the legacy scheme.
type Migration struct {
	ID     string
	OldDir string
	NewDir string
}

// PlanMigration returns moves for accounts that still use legacy paths.
func PlanMigration(stored, root, platform, id, legacy string) (Migration, bool) {
	if strings.TrimSpace(stored) != "" {
		return Migration{}, false
	}
	if legacy == "" {
		return Migration{}, false
	}
	if st, err := os.Stat(legacy); err != nil || !st.IsDir() {
		return Migration{}, false
	}
	newDir := filepath.Join(root, Name(platform, id))
	if _, err := os.Stat(newDir); err == nil {
		return Migration{}, false
	}
	return Migration{ID: id, OldDir: legacy, NewDir: newDir}, true
}
