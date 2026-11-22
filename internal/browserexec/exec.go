// Package browserexec starts browser commands on either a shared virtual
// display (when DISPLAY is set) or a private xvfb-run instance.
package browserexec

import (
	"os"
	"os/exec"
)

const screen = "1366x900x24"

// Command builds an exec.Cmd for bin+args. When DISPLAY is present the command
// runs directly so many browsers can share one Xvfb. Otherwise it falls back to
// xvfb-run for standalone scripts.
func Command(xvfbRun, bin string, args ...string) *exec.Cmd {
	if os.Getenv("DISPLAY") != "" {
		return exec.Command(bin, args...)
	}
	if xvfbRun == "" {
		xvfbRun = "xvfb-run"
	}
	all := append([]string{"-a", "-s", "-screen 0 " + screen, bin}, args...)
	return exec.Command(xvfbRun, all...)
}
