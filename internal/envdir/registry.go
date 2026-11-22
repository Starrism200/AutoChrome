package envdir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Registry is a per-user index of environment directories. It makes
// environments resolvable after the user changes env_root or opens an
// environment stored under an older root.
type Registry struct {
	path string
	mu   sync.Mutex
}

// NewRegistry returns a registry backed by path.
func NewRegistry(path string) *Registry {
	return &Registry{path: path}
}

// List returns every registered environment.
func (r *Registry) List() ([]Meta, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load()
}

// Get returns an environment by id.
func (r *Registry) Get(id string) (Meta, bool) {
	list, err := r.List()
	if err != nil {
		return Meta{}, false
	}
	for _, m := range list {
		if m.ID == id {
			return m, true
		}
	}
	return Meta{}, false
}

// Put inserts or replaces an environment entry.
func (r *Registry) Put(meta Meta) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	if err != nil {
		return err
	}
	found := false
	for i := range list {
		if list[i].ID == meta.ID {
			list[i] = meta
			found = true
			break
		}
	}
	if !found {
		list = append(list, meta)
	}
	return r.save(list)
}

// Delete removes an environment entry.
func (r *Registry) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.load()
	if err != nil {
		return err
	}
	out := list[:0]
	for _, m := range list {
		if m.ID != id {
			out = append(out, m)
		}
	}
	return r.save(out)
}

func (r *Registry) load() ([]Meta, error) {
	b, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Meta{}, nil
		}
		return nil, err
	}
	var list []Meta
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (r *Registry) save(list []Meta) error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, b, 0o600)
}
