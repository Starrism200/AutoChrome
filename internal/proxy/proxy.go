// Package proxy stores per-user proxy definitions and small health checks.
package proxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Proxy is one configured proxy.
type Proxy struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	URL         string   `json:"url"`
	Enabled     bool     `json:"enabled"`
	Tags        []string `json:"tags,omitempty"`
	CreatedAt   string   `json:"created_at,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
	LastCheckAt string   `json:"last_check_at,omitempty"`
	LastCheckOK bool     `json:"last_check_ok,omitempty"`
	LastError   string   `json:"last_error,omitempty"`
}

// Store persists proxies as JSON for one user.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a store backed by path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// List returns all proxies sorted by creation time.
func (s *Store) List() ([]Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Get returns one proxy by id.
func (s *Store) Get(id string) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return Proxy{}, err
	}
	for _, p := range list {
		if p.ID == id {
			return p, nil
		}
	}
	return Proxy{}, fmt.Errorf("proxy %q not found", id)
}

// Add validates and stores a new proxy.
func (s *Store) Add(p Proxy) (Proxy, error) {
	if err := ValidateURL(p.URL); err != nil {
		return Proxy{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return Proxy{}, err
	}
	if p.ID == "" {
		p.ID = newID()
	}
	for _, existing := range list {
		if existing.ID == p.ID {
			return Proxy{}, fmt.Errorf("proxy %q already exists", p.ID)
		}
	}
	now := time.Now().Format(time.RFC3339)
	p.CreatedAt = now
	p.UpdatedAt = now
	list = append(list, p)
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	if err := s.save(list); err != nil {
		return Proxy{}, err
	}
	return p, nil
}

// Update applies fn to a proxy and saves it.
func (s *Store) Update(id string, fn func(*Proxy) error) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return Proxy{}, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if err := fn(&list[i]); err != nil {
			return Proxy{}, err
		}
		if err := ValidateURL(list[i].URL); err != nil {
			return Proxy{}, err
		}
		list[i].UpdatedAt = time.Now().Format(time.RFC3339)
		if err := s.save(list); err != nil {
			return Proxy{}, err
		}
		return list[i], nil
	}
	return Proxy{}, fmt.Errorf("proxy %q not found", id)
}

// Delete removes a proxy by id.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	out := list[:0]
	found := false
	for _, p := range list {
		if p.ID == id {
			found = true
			continue
		}
		out = append(out, p)
	}
	if !found {
		return fmt.Errorf("proxy %q not found", id)
	}
	return s.save(out)
}

// Check performs a TCP reachability check and records the result.
func (s *Store) Check(id string) (Proxy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return Proxy{}, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		addr, err := dialAddr(list[i].URL)
		ok := false
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			conn, derr := net.DialTimeout("tcp", addr, 5*time.Second)
			if derr != nil {
				msg = derr.Error()
			} else {
				ok = true
				_ = conn.Close()
			}
		}
		list[i].LastCheckAt = time.Now().Format(time.RFC3339)
		list[i].LastCheckOK = ok
		list[i].LastError = msg
		if err := s.save(list); err != nil {
			return Proxy{}, err
		}
		return list[i], nil
	}
	return Proxy{}, fmt.Errorf("proxy %q not found", id)
}

func (s *Store) load() ([]Proxy, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Proxy{}, nil
		}
		return nil, err
	}
	var list []Proxy
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) save(list []Proxy) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0o600)
}

// ValidateURL checks the supported proxy formats.
func ValidateURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	if !strings.Contains(raw, "://") {
		if _, _, err := net.SplitHostPort(raw); err != nil {
			return fmt.Errorf("invalid proxy address: %w", err)
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks", "socks5", "socks5h":
	default:
		return fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy host is required")
	}
	return nil
}

func dialAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if _, _, err := net.SplitHostPort(raw); err != nil {
			return "", fmt.Errorf("invalid proxy address: %w", err)
		}
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	host := u.Host
	if host == "" {
		return "", fmt.Errorf("proxy host is required")
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host, nil
	}
	port := "8080"
	switch strings.ToLower(u.Scheme) {
	case "https":
		port = "443"
	case "socks", "socks5", "socks5h":
		port = "1080"
	}
	return net.JoinHostPort(host, port), nil
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("p_%d", time.Now().UnixNano())
	}
	return "p_" + hex.EncodeToString(b)
}
