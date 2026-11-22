// Package jobs runs user scripts asynchronously with logs and cancellation.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"autochrome/internal/atomicfile"
	"autochrome/internal/filelock"
	"autochrome/internal/jsruntime"
	"autochrome/internal/scripts"
	"autochrome/internal/session"
	"autochrome/internal/users"
)

const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSucceeded   = "succeeded"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusInterrupted = "interrupted"

	defaultConcurrency = 10
	retention          = 7 * 24 * time.Hour
)

// Job is one asynchronous script execution.
type Job struct {
	ID         string         `json:"id"`
	Script     string         `json:"script"`
	Status     string         `json:"status"`
	Input      map[string]any `json:"input,omitempty"`
	SessionID  string         `json:"session_id,omitempty"`
	Result     any            `json:"result,omitempty"`
	Error      string         `json:"error,omitempty"`
	CreatedAt  string         `json:"created_at"`
	StartedAt  string         `json:"started_at,omitempty"`
	FinishedAt string         `json:"finished_at,omitempty"`
	LogPath    string         `json:"log_path,omitempty"`
}

// Manager owns jobs for one user.
type Manager struct {
	user        users.User
	sessions    *session.Manager
	allowEval   bool
	indexPath   string
	logDir      string
	concurrency int
	sem         chan struct{}
	publish     func(string, any)

	mu      sync.Mutex
	jobs    map[string]Job
	cancels map[string]context.CancelFunc
	wg      sync.WaitGroup
}

// New loads persisted jobs and marks previously running jobs as interrupted.
func New(user users.User, sessions *session.Manager, allowEval bool, publish func(string, any)) *Manager {
	concurrency := defaultConcurrency
	if v := strings.TrimSpace(os.Getenv("AUTOCHROME_JOB_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			concurrency = n
		}
	}
	m := &Manager{
		user:        user,
		sessions:    sessions,
		allowEval:   allowEval,
		indexPath:   filepath.Join(user.Home, "data", "users", user.ID, "jobs.json"),
		logDir:      filepath.Join(user.LogRoot, "jobs"),
		concurrency: concurrency,
		sem:         make(chan struct{}, concurrency),
		publish:     publish,
		jobs:        map[string]Job{},
		cancels:     map[string]context.CancelFunc{},
	}
	_ = os.MkdirAll(m.logDir, 0o755)
	m.load()
	return m
}

// SetUser updates storage-related user settings for future jobs.
func (m *Manager) SetUser(u users.User) {
	m.mu.Lock()
	m.user = u
	m.mu.Unlock()
}

func (m *Manager) userSnapshot() users.User {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.user
}

// Start validates the script and schedules it.
func (m *Manager) Start(name string, input map[string]any, sessionID string) (Job, error) {
	user := m.userSnapshot()
	script, src, err := scripts.Load(user.ScriptRoot, name)
	if err != nil {
		return Job{}, err
	}
	id := newID()
	logPath := filepath.Join(m.logDir, id+".log")
	runInput := map[string]any{}
	for k, v := range input {
		runInput[k] = v
	}
	if sessionID != "" {
		runInput["session_id"] = sessionID
	}
	job := Job{
		ID: id, Script: name, Status: StatusQueued, Input: input, SessionID: sessionID,
		CreatedAt: time.Now().Format(time.RFC3339), LogPath: logPath,
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.jobs[id] = job
	m.cancels[id] = cancel
	err = m.persistLocked()
	if err != nil {
		delete(m.jobs, id)
		delete(m.cancels, id)
	} else {
		m.wg.Add(1)
	}
	m.mu.Unlock()
	if err != nil {
		cancel()
		return Job{}, err
	}
	m.emit("job.created", job)
	go m.run(ctx, id, src, script.TimeoutDuration(), runInput, logPath)
	return job, nil
}

// List returns jobs newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	return j, ok
}

// Cancel stops a queued or running job.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	cancel, ok := m.cancels[id]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("job %q not found or already finished", id)
	}
	cancel()
	return nil
}

// Logs returns the complete log text.
func (m *Manager) Logs(id string) (string, error) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("job %q not found", id)
	}
	b, err := os.ReadFile(j.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// Close cancels all jobs and waits briefly for them to stop.
func (m *Manager) Close() {
	m.mu.Lock()
	for _, cancel := range m.cancels {
		cancel()
	}
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

func (m *Manager) run(ctx context.Context, id, src string, timeout time.Duration, input map[string]any, logPath string) {
	defer m.wg.Done()
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		m.finish(id, StatusFailed, nil, err)
		return
	}
	defer f.Close()

	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(f, "[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	logf("queued")
	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		logf("cancelled before start")
		m.finish(id, StatusCancelled, nil, ctx.Err())
		return
	}
	defer func() { <-m.sem }()

	m.setRunning(id)
	logf("running")
	runner := jsruntime.New(m.userSnapshot(), m.sessions, f)
	runner.AllowEval = m.allowEval
	runner.AutoClose = true
	runner.Timeout = timeout
	result, runErr := runner.RunContext(ctx, src, input)
	status := StatusSucceeded
	message := ""
	if runErr != nil {
		status = StatusFailed
		message = runErr.Error()
		if ctx.Err() != nil {
			status = StatusCancelled
			message = "cancelled"
		}
	}
	logf("finished: %s %s", status, message)
	m.finishResult(id, status, result, message)
}

func (m *Manager) setRunning(id string) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	j.Status = StatusRunning
	j.StartedAt = time.Now().Format(time.RFC3339)
	m.jobs[id] = j
	_ = m.persistLocked()
	m.mu.Unlock()
	m.emit("job.updated", j)
}

func (m *Manager) finish(id, status string, result any, err error) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	m.finishResult(id, status, result, message)
}

func (m *Manager) finishResult(id, status string, result any, message string) {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	j.Status = status
	j.Result = result
	j.Error = message
	j.FinishedAt = time.Now().Format(time.RFC3339)
	if j.StartedAt == "" {
		j.StartedAt = j.FinishedAt
	}
	delete(m.cancels, id)
	m.jobs[id] = j
	_ = m.persistLocked()
	m.mu.Unlock()
	m.emit("job.updated", j)
}

func (m *Manager) emit(typ string, data any) {
	if m.publish != nil {
		m.publish(typ, data)
	}
}

func (m *Manager) load() {
	b, err := os.ReadFile(m.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		return
	}
	var list []Job
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}
	cutoff := time.Now().Add(-retention)
	changed := false
	for _, j := range list {
		if j.Status == StatusQueued || j.Status == StatusRunning {
			j.Status = StatusInterrupted
			j.Error = "server restarted"
			j.FinishedAt = time.Now().Format(time.RFC3339)
			changed = true
		}
		if j.FinishedAt != "" {
			if t, err := time.Parse(time.RFC3339, j.FinishedAt); err == nil && t.Before(cutoff) {
				if j.LogPath != "" {
					_ = os.Remove(j.LogPath)
				}
				changed = true
				continue
			}
		}
		m.jobs[j.ID] = j
	}
	if changed {
		m.mu.Lock()
		_ = m.persistLocked()
		m.mu.Unlock()
	}
}

func (m *Manager) persistLocked() error {
	list := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		list = append(list, j)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	lock, err := filelock.Acquire(m.indexPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	return atomicfile.Write(m.indexPath, b, 0o600)
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("j_%d", time.Now().UnixNano())
	}
	return "j_" + hex.EncodeToString(b)
}
