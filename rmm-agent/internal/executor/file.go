package executor

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// writeFile writes content to path idempotently: it compares the SHA-256 of the
// existing file to the desired content and only rewrites on difference. Writes
// are atomic (temp file in the same directory, then rename). Mode/owner are
// reconciled even when content is unchanged.
func writeFile(path, content, mode, owner, group string) StepResult {
	res := StepResult{OK: true}
	if path == "" {
		res.OK = false
		res.Err = "file.write: missing 'path'"
		return res
	}

	desired := sha256.Sum256([]byte(content))
	if existing, err := os.ReadFile(path); err == nil && sha256.Sum256(existing) == desired {
		// Content already matches; reconcile mode/owner only.
		res.Output = fmt.Sprintf("%s already up to date (%d bytes)", path, len(content))
		if reconcileMode(path, mode, &res) {
			res.Changed = true
		}
		applyOwner(path, owner, group, &res)
		return res
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".linexus-*")
	if err != nil {
		res.OK = false
		res.Err = fmt.Sprintf("file.write: create temp: %v", err)
		return res
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		res.OK = false
		res.Err = fmt.Sprintf("file.write: %v", err)
		return res
	}
	tmp.Close()

	if m, ok := parseMode(mode); ok {
		_ = os.Chmod(tmpName, m)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		res.OK = false
		res.Err = fmt.Sprintf("file.write: rename: %v", err)
		return res
	}

	res.Changed = true
	res.Output = fmt.Sprintf("wrote %s (%d bytes)", path, len(content))
	applyOwner(path, owner, group, &res)
	return res
}

// reconcileMode applies mode if it differs from the current permissions.
// Returns true if it changed anything.
func reconcileMode(path, mode string, res *StepResult) bool {
	m, ok := parseMode(mode)
	if !ok {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() == m {
		return false
	}
	if err := os.Chmod(path, m); err != nil {
		res.Output += fmt.Sprintf("; chmod failed: %v", err)
		return false
	}
	res.Output += fmt.Sprintf("; mode -> %s", mode)
	return true
}

// parseMode accepts "0644", "0o644", or "644" as octal.
func parseMode(s string) (os.FileMode, bool) {
	if s == "" {
		return 0, false
	}
	s = strings.TrimPrefix(s, "0o")
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, false
	}
	return os.FileMode(v), true
}

func applyOwner(path, owner, group string, res *StepResult) {
	if owner == "" && group == "" {
		return
	}
	spec := owner
	if group != "" {
		spec = owner + ":" + group
	}
	if out, err := runPrivileged("chown", spec, path); err != nil {
		res.Output += fmt.Sprintf("; chown failed: %s", strings.TrimSpace(out+" "+err.Error()))
	}
}
