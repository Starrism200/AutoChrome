// Package attachments stores generic user files (avatars, uploads, exports)
// under a configurable root.
package attachments

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// File is one stored attachment.
type File struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	CreatedAt string `json:"created_at"`
}

// Store persists attachments and an index file.
type Store struct {
	indexPath string
	mu        sync.Mutex
	root      string
}

// NewStore returns a store with root and indexPath.
func NewStore(root, indexPath string) *Store {
	return &Store{root: root, indexPath: indexPath}
}

// SetRoot changes the storage root for new files.
func (s *Store) SetRoot(root string) {
	s.mu.Lock()
	s.root = root
	s.mu.Unlock()
}

// Root returns the current storage root.
func (s *Store) Root() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.root
}

// List returns all attachments sorted by creation time.
func (s *Store) List() ([]File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return nil, err
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	return list, nil
}

// Get returns one attachment by id.
func (s *Store) Get(id string) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return File{}, err
	}
	for _, f := range list {
		if f.ID == id {
			return f, nil
		}
	}
	return File{}, fmt.Errorf("attachment %q not found", id)
}

// Save copies r into the store, enforcing max bytes.
func (s *Store) Save(name string, r io.Reader, max int64) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return File{}, err
	}
	id := newID()
	safe := safeName(name)
	if safe == "" {
		safe = "file"
	}
	path := filepath.Join(s.root, id+"-"+safe)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return File{}, err
	}
	h := sha256.New()
	limit := max
	if limit <= 0 {
		limit = 512 << 20
	}
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return File{}, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return File{}, closeErr
	}
	if n > limit {
		_ = os.Remove(path)
		return File{}, fmt.Errorf("file exceeds %d bytes", limit)
	}
	file := File{
		ID: id, Name: safe, Path: path, Size: n,
		MIME:      detectMIME(safe),
		SHA256:    hex.EncodeToString(h.Sum(nil)),
		CreatedAt: time.Now().Format(time.RFC3339),
	}
	list, err := s.load()
	if err != nil {
		_ = os.Remove(path)
		return File{}, err
	}
	list = append(list, file)
	if err := s.save(list); err != nil {
		_ = os.Remove(path)
		return File{}, err
	}
	return file, nil
}

// Rename changes the display name of an attachment.
func (s *Store) Rename(id, name string) (File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return File{}, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		safe := safeName(name)
		if safe == "" {
			return File{}, fmt.Errorf("name is required")
		}
		list[i].Name = safe
		if err := s.save(list); err != nil {
			return File{}, err
		}
		return list[i], nil
	}
	return File{}, fmt.Errorf("attachment %q not found", id)
}

// Delete removes an attachment and its file.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.load()
	if err != nil {
		return err
	}
	out := list[:0]
	found := false
	var path string
	for _, f := range list {
		if f.ID == id {
			found = true
			path = f.Path
			continue
		}
		out = append(out, f)
	}
	if !found {
		return fmt.Errorf("attachment %q not found", id)
	}
	if path != "" {
		_ = os.Remove(path)
	}
	return s.save(out)
}

func (s *Store) load() ([]File, error) {
	b, err := os.ReadFile(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return []File{}, nil
		}
		return nil, err
	}
	var list []File
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) save(list []File) error {
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath, b, 0o600)
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = unsafe.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._-")
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}

func detectMIME(name string) string {
	if typ := mime.TypeByExtension(filepath.Ext(name)); typ != "" {
		return typ
	}
	return "application/octet-stream"
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("a_%d", time.Now().UnixNano())
	}
	return "a_" + hex.EncodeToString(b)
}
