// Package scheduler stores simple interval-based jobs for the HTTP API.
package scheduler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"autochrome/internal/atomicfile"
	"autochrome/internal/filelock"
)

// Job is one scheduled action.
type Job struct {
	ID              string         `json:"id"`
	Name            string         `json:"name,omitempty"`
	Type            string         `json:"type"` // script | open | close
	Env             string         `json:"env,omitempty"`
	URL             string         `json:"url,omitempty"`
	ProxyID         string         `json:"proxy_id,omitempty"`
	ChromeVersion   string         `json:"chrome_version,omitempty"`
	Script          string         `json:"script,omitempty"`
	Input           map[string]any `json:"input,omitempty"`
	IntervalSeconds int            `json:"interval_seconds"`
	Enabled         bool           `json:"enabled"`
	CreatedAt       string         `json:"created_at,omitempty"`
	LastRun         string         `json:"last_run,omitempty"`
	LastStatus      string         `json:"last_status,omitempty"`
	LastError       string         `json:"last_error,omitempty"`
	NextRun         string         `json:"next_run,omitempty"`
}

// Store persists jobs for one user.
type Store struct {
	path string
	mu   sync.Mutex
}

// NewStore returns a store backed by path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// List returns every job.
func (s *Store) List() ([]Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// Get returns one job.
func (s *Store) Get(id string) (Job, error) {
	list, err := s.List()
	if err != nil {
		return Job{}, err
	}
	for _, j := range list {
		if j.ID == id {
			return j, nil
		}
	}
	return Job{}, fmt.Errorf("schedule %q not found", id)
}

// Add creates a job.
func (s *Store) Add(j Job) (Job, error) {
	if j.IntervalSeconds <= 0 {
		return Job{}, fmt.Errorf("interval_seconds must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := filelock.Acquire(s.path)
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	list, err := s.load()
	if err != nil {
		return Job{}, err
	}
	if j.ID == "" {
		j.ID = newID()
	}
	j.CreatedAt = time.Now().Format(time.RFC3339)
	if j.NextRun == "" {
		j.NextRun = time.Now().Format(time.RFC3339)
	}
	list = append(list, j)
	if err := s.save(list); err != nil {
		return Job{}, err
	}
	return j, nil
}

// Update applies fn to a job.
func (s *Store) Update(id string, fn func(*Job) error) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := filelock.Acquire(s.path)
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	list, err := s.load()
	if err != nil {
		return Job{}, err
	}
	for i := range list {
		if list[i].ID != id {
			continue
		}
		if err := fn(&list[i]); err != nil {
			return Job{}, err
		}
		if list[i].IntervalSeconds <= 0 {
			return Job{}, fmt.Errorf("interval_seconds must be positive")
		}
		if err := s.save(list); err != nil {
			return Job{}, err
		}
		return list[i], nil
	}
	return Job{}, fmt.Errorf("schedule %q not found", id)
}

// Delete removes a job.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, err := filelock.Acquire(s.path)
	if err != nil {
		return err
	}
	defer lock.Close()
	list, err := s.load()
	if err != nil {
		return err
	}
	out := list[:0]
	found := false
	for _, j := range list {
		if j.ID == id {
			found = true
			continue
		}
		out = append(out, j)
	}
	if !found {
		return fmt.Errorf("schedule %q not found", id)
	}
	return s.save(out)
}

// Due returns enabled jobs whose NextRun has passed.
func (s *Store) Due(now time.Time) ([]Job, error) {
	list, err := s.List()
	if err != nil {
		return nil, err
	}
	out := []Job{}
	for _, j := range list {
		if !j.Enabled {
			continue
		}
		if j.NextRun == "" {
			out = append(out, j)
			continue
		}
		next, err := time.Parse(time.RFC3339, j.NextRun)
		if err != nil || !next.After(now) {
			out = append(out, j)
		}
	}
	return out, nil
}

// MarkRun records an execution result and computes the next run.
func (s *Store) MarkRun(id, status, errMsg string, next time.Time) (Job, error) {
	return s.Update(id, func(j *Job) error {
		j.LastRun = time.Now().Format(time.RFC3339)
		j.LastStatus = status
		j.LastError = errMsg
		j.NextRun = next.Format(time.RFC3339)
		return nil
	})
}

func (s *Store) load() ([]Job, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Job{}, nil
		}
		return nil, err
	}
	var list []Job
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Store) save(list []Job) error {
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.Write(s.path, b, 0o600)
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("j_%d", time.Now().UnixNano())
	}
	return "j_" + hex.EncodeToString(b)
}
