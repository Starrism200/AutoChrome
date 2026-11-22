// Package xvfb starts and owns a private Xvfb display for long-lived browser
// sessions. When DISPLAY is already set, Start returns a no-op handle.
package xvfb

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Display is a running Xvfb instance (or an existing external display).
type Display struct {
	Name string
	cmd  *exec.Cmd
}

// Start returns a display for browser sessions. If DISPLAY is set, it is used
// as-is. Otherwise a free display number between :99 and :198 is started.
func Start(screen string) (*Display, error) {
	if screen == "" {
		screen = "1920x1080x24"
	}
	if name := os.Getenv("DISPLAY"); name != "" {
		return &Display{Name: name}, nil
	}
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		return nil, fmt.Errorf("Xvfb not found; run ./install.sh or set DISPLAY")
	}
	for n := 99; n < 199; n++ {
		name := fmt.Sprintf(":%d", n)
		if _, err := os.Stat(filepath.Join("/tmp/.X11-unix", fmt.Sprintf("X%d", n))); err == nil {
			continue
		}
		cmd := exec.Command(bin, name, "-screen", "0", screen, "-nolisten", "tcp")
		cmd.Stdout = nil
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			continue
		}
		d := &Display{Name: name, cmd: cmd}
		if d.waitReady(3 * time.Second) {
			return d, nil
		}
		_ = d.Close()
	}
	return nil, fmt.Errorf("could not start Xvfb")
}

// Env returns the environment entries needed to use this display.
func (d *Display) Env() []string {
	if d == nil || d.Name == "" {
		return nil
	}
	return []string{"DISPLAY=" + d.Name}
}

// Close stops the Xvfb process if this handle started it.
func (d *Display) Close() error {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = d.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = d.cmd.Process.Kill()
		<-done
	}
	return nil
}

func (d *Display) waitReady(timeout time.Duration) bool {
	socket := filepath.Join("/tmp/.X11-unix", "X"+d.Name[1:])
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return true
		}
		if d.cmd.ProcessState != nil && d.cmd.ProcessState.Exited() {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
