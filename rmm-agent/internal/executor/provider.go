package executor

import (
	"os"
	"os/exec"
)

// PkgManager is the detected system package manager.
type PkgManager int

const (
	PkgUnknown PkgManager = iota
	PkgApt
	PkgDnf
)

func detectPkgManager() PkgManager {
	if _, err := exec.LookPath("apt-get"); err == nil {
		return PkgApt
	}
	if _, err := exec.LookPath("dnf"); err == nil {
		return PkgDnf
	}
	if _, err := exec.LookPath("yum"); err == nil {
		return PkgDnf
	}
	return PkgUnknown
}

// hasSystemd reports whether systemd is the running init system (not merely
// that systemctl is on PATH).
func hasSystemd() bool {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return false
	}
	if st, err := os.Stat("/run/systemd/system"); err == nil && st.IsDir() {
		return true
	}
	return false
}
