package facts

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Seams for tests: the commands collectors run and where they look.
var (
	// runTool runs a read-only tool and returns its stdout. It is bounded by a
	// timeout so one hung tool cannot stall a report.
	runTool = func(name string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=")
		out, err := cmd.Output()
		return string(out), err
	}
	lookPath = exec.LookPath
	// systemdRunning reports whether systemd is the running init system.
	systemdRunning = func() bool {
		if _, err := lookPath("systemctl"); err != nil {
			return false
		}
		st, err := os.Stat("/run/systemd/system")
		return err == nil && st.IsDir()
	}
	// procRoot is where /proc is mounted.
	procRoot = "/proc"
)

func trimLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
