package executor

import (
	"fmt"
	"os/exec"
	"strings"
)

func serviceActive(name string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", name).Run() == nil
}

func serviceEnabled(name string) bool {
	return exec.Command("systemctl", "is-enabled", "--quiet", name).Run() == nil
}

// ensureService converges a service's run state (started/stopped) and enable
// state. Each is checked natively first and only changed on drift.
func ensureService(name, runState, enabled string) StepResult {
	res := StepResult{OK: true}
	if name == "" {
		res.OK = false
		res.Err = "service.ensure: missing 'name'"
		return res
	}
	if !hasSystemd() {
		res.OK = false
		res.Err = "service.ensure: systemd not available on this host"
		return res
	}

	var notes []string

	if runState != "" {
		want := runState == "started" || runState == "running" || runState == "active"
		active := serviceActive(name)
		switch {
		case want && !active:
			if out, err := runPrivileged("systemctl", "start", name); err != nil {
				return fail(res, "start", out, err)
			}
			res.Changed = true
			notes = append(notes, "started")
		case !want && active:
			if out, err := runPrivileged("systemctl", "stop", name); err != nil {
				return fail(res, "stop", out, err)
			}
			res.Changed = true
			notes = append(notes, "stopped")
		default:
			notes = append(notes, fmt.Sprintf("run-state already %s", runState))
		}
	}

	if enabled != "" {
		want := enabled == "true"
		isEnabled := serviceEnabled(name)
		switch {
		case want && !isEnabled:
			if out, err := runPrivileged("systemctl", "enable", name); err != nil {
				return fail(res, "enable", out, err)
			}
			res.Changed = true
			notes = append(notes, "enabled")
		case !want && isEnabled:
			if out, err := runPrivileged("systemctl", "disable", name); err != nil {
				return fail(res, "disable", out, err)
			}
			res.Changed = true
			notes = append(notes, "disabled")
		default:
			notes = append(notes, fmt.Sprintf("enable-state already %v", want))
		}
	}

	res.Output = strings.Join(notes, "; ")
	return res
}

func fail(res StepResult, op, out string, err error) StepResult {
	res.OK = false
	res.Err = fmt.Sprintf("%s: %s", op, strings.TrimSpace(out+" "+err.Error()))
	return res
}
