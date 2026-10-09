package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
)

// writeAtomic writes content to path only when it differs, via a temp file in
// the same directory and a rename, so readers (named, mount) never see half a
// file. When the agent may not write there itself it stages the file in the
// temp dir and installs it with root (execPriv). It reports whether it wrote.
func writeAtomic(path string, content []byte, mode os.FileMode) (bool, error) {
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, content) {
		return false, nil
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".linexus-*")
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return true, installPrivileged(path, content, mode)
		}
		return false, err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after the rename
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Chmod(name, mode); err != nil {
		return false, err
	}
	if err := os.Rename(name, path); err != nil {
		return false, err
	}
	return true, nil
}

func installPrivileged(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp("", "linexus-stage-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	if out, err := execPriv("install", "-m", fmt.Sprintf("%04o", mode.Perm()), tmp.Name(), path); err != nil {
		return fmt.Errorf("install %s: %v %s", path, err, out)
	}
	return nil
}

// ensureDir creates dir (and parents) when missing, with root when needed.
// It reports whether it created anything.
func ensureDir(dir string, mode os.FileMode) (bool, error) {
	if st, err := os.Stat(dir); err == nil {
		if !st.IsDir() {
			return false, fmt.Errorf("%s exists and is not a directory", dir)
		}
		return false, nil
	}
	if err := os.MkdirAll(dir, mode); err != nil {
		if !errors.Is(err, fs.ErrPermission) {
			return false, err
		}
		if out, err := execPriv("mkdir", "-p", "-m", fmt.Sprintf("%04o", mode.Perm()), dir); err != nil {
			return false, fmt.Errorf("mkdir %s: %v %s", dir, err, out)
		}
	}
	return true, nil
}

// chownToService hands path to a service account (bind, named) when that
// account exists. Best effort: a failure is returned as a note, not an error.
func chownToService(path, account string) string {
	if account == "" {
		return ""
	}
	if _, err := user.Lookup(account); err != nil {
		return ""
	}
	if out, err := execPriv("chown", account+":"+account, path); err != nil {
		return fmt.Sprintf("chown %s %s failed: %v %s", account, path, err, out)
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
