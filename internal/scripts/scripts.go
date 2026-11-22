// Package scripts discovers user JavaScript scripts and their optional
// manifest files.
package scripts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Script is one user script.
type Script struct {
	Name        string         `json:"name"`
	Path        string         `json:"path"`
	Description string         `json:"description,omitempty"`
	Timeout     string         `json:"timeout,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
	timeout     time.Duration
}

// TimeoutDuration returns the parsed timeout (default 60s).
func (s Script) TimeoutDuration() time.Duration {
	if s.timeout > 0 {
		return s.timeout
	}
	return 60 * time.Second
}

type manifest struct {
	Description string          `json:"description"`
	Timeout     json.RawMessage `json:"timeout"`
	InputSchema map[string]any  `json:"inputSchema"`
}

// List returns every *.js file in root, sorted by name.
func List(root string) ([]Script, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []Script{}, nil
		}
		return nil, err
	}
	out := []Script{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".js")
		if !namePattern.MatchString(name) {
			continue
		}
		s, err := load(root, name)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Load returns the script source for name.
func Load(root, name string) (Script, string, error) {
	if !namePattern.MatchString(name) {
		return Script{}, "", fmt.Errorf("invalid script name %q", name)
	}
	s, err := load(root, name)
	if err != nil {
		return Script{}, "", err
	}
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return Script{}, "", err
	}
	return s, string(b), nil
}

func load(root, name string) (Script, error) {
	path := filepath.Join(root, name+".js")
	if _, err := os.Stat(path); err != nil {
		return Script{}, fmt.Errorf("script %q not found in %s", name, root)
	}
	s := Script{Name: name, Path: path, Timeout: "60s"}
	b, err := os.ReadFile(filepath.Join(root, name+".json"))
	if err == nil {
		var m manifest
		if err := json.Unmarshal(b, &m); err == nil {
			s.Description = m.Description
			s.InputSchema = m.InputSchema
			if d, ok := parseTimeout(m.Timeout); ok {
				s.timeout = d
				s.Timeout = d.String()
			}
		}
	}
	return s, nil
}

func parseTimeout(raw json.RawMessage) (time.Duration, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if d, err := time.ParseDuration(asString); err == nil && d > 0 {
			return d, true
		}
		return 0, false
	}
	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil && asNumber > 0 {
		return time.Duration(asNumber * float64(time.Second)), true
	}
	return 0, false
}
