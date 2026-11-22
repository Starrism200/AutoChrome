// Package browserslot limits how many browsers run at once across processes.
//
// The limit is enforced with advisory flock on a fixed set of slot files, so
// multiple processes share the same cap.
package browserslot

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Dir returns the slot directory.
func Dir(defaultDir string) string {
	if v := strings.TrimSpace(os.Getenv("AUTOCHROME_SLOT_DIR")); v != "" {
		return v
	}
	return defaultDir
}

// Max returns the configured browser cap (default 10).
func Max() int {
	if v := strings.TrimSpace(os.Getenv("AUTOCHROME_BROWSER_MAX")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 10
}

// Acquire reserves one slot. It blocks until a slot is free or wait elapses.
func Acquire(dir string, max int, wait time.Duration) (func(), error) {
	if max <= 0 {
		max = Max()
	}
	if dir == "" {
		dir = Dir("data/browser-slots")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		for i := 0; i < max; i++ {
			p := filepath.Join(dir, fmt.Sprintf("slot-%02d.lock", i))
			f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				continue
			}
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				f.Close()
				continue
			}
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("浏览器并发已满（%d），请稍后重试", max)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
