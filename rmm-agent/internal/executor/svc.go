package executor

import (
	"fmt"
	"strings"
)

func serviceActive(name string) bool {
	_, err := execPlain("systemctl", "is-active", "--quiet", name)
	return err == nil
}

func serviceEnabled(name string) bool {
	_, err := execPlain("systemctl", "is-enabled", "--quiet", name)
	return err == nil
}

// ensureServiceStep is service.ensure: params name (or service), state
// (started|stopped), enabled (true|false), restart ("true" restarts the
// service unconditionally — the one non-idempotent verb, which always
// reports changed).
func ensureServiceStep(p map[string]string) StepResult {
	name := p["name"]
	if name == "" {
		name = p["service"]
	}
	restart := strings.EqualFold(p["restart"], "true")
	if restart && (p["state"] == "stopped" || p["state"] == "inactive") {
		return StepResult{Err: "service.ensure: restart=true conflicts with state=stopped"}
	}
	return ensureService(name, p["state"], p["enabled"], restart)
}

// ensureService converges a service's run state (started/stopped) and enable
// state. Each is checked natively first and only changed on drift. restart
// restarts it (or starts it when it was not running).
func ensureService(name, runState, enabled string, restart bool) StepResult {
	res := StepResult{OK: true}
	if name == "" {
		res.OK = false
		res.Err = "service.ensure: missing 'name'"
		return res
	}
	if !validUnitName(name) {
		res.OK = false
		res.Err = fmt.Sprintf("service.ensure: %q is not a unit name", name)
		return res
	}
	if !systemdPresent() {
		res.OK = false
		res.Err = "service.ensure: systemd not available on this host"
		return res
	}

	var notes []string

	if restart {
		if out, err := execPriv("systemctl", "restart", name); err != nil {
			return fail(res, "restart", out, err)
		}
		res.Changed = true
		notes = append(notes, "restarted")
	} else if runState != "" {
		want := runState == "started" || runState == "running" || runState == "active"
		active := serviceActive(name)
		switch {
		case want && !active:
			if out, err := execPriv("systemctl", "start", name); err != nil {
				return fail(res, "start", out, err)
			}
			res.Changed = true
			notes = append(notes, "started")
		case !want && active:
			if out, err := execPriv("systemctl", "stop", name); err != nil {
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
			if out, err := execPriv("systemctl", "enable", name); err != nil {
				return fail(res, "enable", out, err)
			}
			res.Changed = true
			notes = append(notes, "enabled")
		case !want && isEnabled:
			if out, err := execPriv("systemctl", "disable", name); err != nil {
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

// validUnitName accepts systemd unit names (letters, digits, and : - _ . \ @),
// never an option ("-…") or a path.
func validUnitName(name string) bool {
	if name == "" || len(name) > 256 || name[0] == '-' {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune(":-_.\\@", r)) {
			return false
		}
	}
	return true
}
