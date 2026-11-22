// Package desktop exposes the Xvfb display over VNC and noVNC so a human can
// watch and take over a browser session (QR scan, SMS code, CAPTCHA).
package desktop

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"autochrome/internal/xvfb"
)

const (
	// DefaultPortStart and DefaultPortEnd bound the auto-assigned VNC and
	// noVNC ports.
	DefaultPortStart = 10000
	DefaultPortEnd   = 59999
)

// Config controls a desktop instance.
type Config struct {
	Screen    string
	PortStart int
	PortEnd   int
	Password  string
	StatePath string
	LogDir    string
	NovncWeb  string
}

// Info is the serializable state of a desktop instance.
type Info struct {
	Display   string `json:"display"`
	RFBPort   int    `json:"rfb_port"`
	WebPort   int    `json:"web_port"`
	URL       string `json:"url"`
	Password  bool   `json:"password"`
	StartedAt string `json:"started_at"`
}

// Desktop owns Xvfb (when it started it), x11vnc and websockify.
type Desktop struct {
	info      Info
	display   *xvfb.Display
	x11vnc    *exec.Cmd
	websocket *exec.Cmd
	passFile  string
	statePath string
	once      sync.Once
}

// Start launches x11vnc on the display and serves noVNC through websockify.
func Start(cfg Config) (*Desktop, error) {
	if cfg.Screen == "" {
		cfg.Screen = "1920x1080x24"
	}
	if cfg.PortStart <= 0 {
		cfg.PortStart = DefaultPortStart
	}
	if cfg.PortEnd <= 0 {
		cfg.PortEnd = DefaultPortEnd
	}
	if cfg.PortEnd < cfg.PortStart {
		return nil, fmt.Errorf("invalid VNC port range %d-%d", cfg.PortStart, cfg.PortEnd)
	}
	if cfg.NovncWeb == "" {
		cfg.NovncWeb = findNoVNC()
	}
	if cfg.NovncWeb == "" {
		return nil, fmt.Errorf("noVNC not found; install the novnc package or run ./install.sh")
	}
	if _, err := exec.LookPath("x11vnc"); err != nil {
		return nil, fmt.Errorf("x11vnc not found; run ./install.sh")
	}
	if _, err := exec.LookPath("websockify"); err != nil {
		return nil, fmt.Errorf("websockify not found; run ./install.sh")
	}

	display, err := xvfb.Start(cfg.Screen)
	if err != nil {
		return nil, err
	}

	rfbPort, err := freePort(cfg.PortStart, cfg.PortEnd, 0)
	if err != nil {
		_ = display.Close()
		return nil, err
	}
	webPort, err := freePort(cfg.PortStart, cfg.PortEnd, rfbPort)
	if err != nil {
		_ = display.Close()
		return nil, err
	}

	passFile := ""
	if cfg.Password != "" {
		passFile, err = writePassFile(cfg.Password)
		if err != nil {
			_ = display.Close()
			return nil, err
		}
	}

	d := &Desktop{
		display:   display,
		passFile:  passFile,
		statePath: cfg.StatePath,
		info: Info{
			Display:   display.Name,
			RFBPort:   rfbPort,
			WebPort:   webPort,
			URL:       fmt.Sprintf("http://127.0.0.1:%d/vnc.html?autoconnect=1&resize=scale&reconnect=1", webPort),
			Password:  passFile != "",
			StartedAt: time.Now().Format(time.RFC3339),
		},
	}

	x11Args := []string{
		"-display", display.Name,
		"-localhost",
		"-rfbport", strconv.Itoa(rfbPort),
		"-forever", "-shared", "-quiet", "-noxdamage",
		"-skip_lockkeys",
	}
	if passFile != "" {
		x11Args = append(x11Args, "-rfbauth", passFile)
	} else {
		x11Args = append(x11Args, "-nopw")
	}
	d.x11vnc = exec.Command("x11vnc", x11Args...)
	d.x11vnc.Stdout = io.Discard
	d.x11vnc.Stderr = logWriter(cfg.LogDir, "x11vnc.log")
	if err := d.x11vnc.Start(); err != nil {
		d.cleanup()
		return nil, fmt.Errorf("start x11vnc: %w", err)
	}
	if !waitPort(fmt.Sprintf("127.0.0.1:%d", rfbPort), 5*time.Second) {
		d.cleanup()
		return nil, fmt.Errorf("x11vnc did not listen on %d", rfbPort)
	}

	d.websocket = exec.Command("websockify",
		"--web="+cfg.NovncWeb,
		fmt.Sprintf("127.0.0.1:%d", webPort),
		fmt.Sprintf("127.0.0.1:%d", rfbPort),
	)
	d.websocket.Stdout = io.Discard
	d.websocket.Stderr = logWriter(cfg.LogDir, "websockify.log")
	if err := d.websocket.Start(); err != nil {
		d.cleanup()
		return nil, fmt.Errorf("start websockify: %w", err)
	}
	if !waitPort(fmt.Sprintf("127.0.0.1:%d", webPort), 5*time.Second) {
		d.cleanup()
		return nil, fmt.Errorf("websockify did not listen on %d", webPort)
	}

	d.writeState()
	return d, nil
}

// Info returns the current desktop endpoints.
func (d *Desktop) Info() Info {
	if d == nil {
		return Info{}
	}
	return d.info
}

// Display returns the Xvfb handle used by this desktop.
func (d *Desktop) Display() *xvfb.Display {
	if d == nil {
		return nil
	}
	return d.display
}

// Close stops websockify, x11vnc, and Xvfb when this instance started it.
func (d *Desktop) Close() error {
	if d == nil {
		return nil
	}
	var err error
	d.once.Do(func() { err = d.cleanup() })
	return err
}

func (d *Desktop) cleanup() error {
	stop(d.websocket)
	stop(d.x11vnc)
	if d.passFile != "" {
		_ = os.RemoveAll(filepath.Dir(d.passFile))
	}
	if d.statePath != "" {
		_ = os.Remove(d.statePath)
	}
	if d.display != nil {
		_ = d.display.Close()
	}
	return nil
}

func (d *Desktop) writeState() {
	if d.statePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(d.statePath), 0o755)
	b, err := json.MarshalIndent(d.info, "", "  ")
	if err == nil {
		_ = os.WriteFile(d.statePath, b, 0o644)
	}
}

func stop(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func freePort(start, end, exclude int) (int, error) {
	for p := start; p <= end; p++ {
		if p == exclude {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			_ = ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %d-%d", start, end)
}

func waitPort(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func findNoVNC() string {
	candidates := []string{
		"/usr/share/novnc",
		"/usr/share/webapps/novnc",
		"/usr/local/share/novnc",
		"/opt/novnc",
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "vnc.html")); err == nil {
			return dir
		}
	}
	return ""
}

func writePassFile(password string) (string, error) {
	dir, err := os.MkdirTemp("", "autochrome-vnc-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "passwd")
	cmd := exec.Command("x11vnc", "-storepasswd", password, path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("store VNC password: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	return path, nil
}

func logWriter(dir, name string) io.Writer {
	if dir == "" {
		return io.Discard
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return io.Discard
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return io.Discard
	}
	return f
}
