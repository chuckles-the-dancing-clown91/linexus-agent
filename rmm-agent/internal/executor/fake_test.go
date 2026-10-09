package executor

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/bind"
)

// fakeHost replaces every host seam with a scripted, recording fake.
type fakeHost struct {
	mu      sync.Mutex
	calls   []string
	handler func(argv []string) (string, error)
}

func (f *fakeHost) run(argv []string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, strings.Join(argv, " "))
	f.mu.Unlock()
	if f.handler == nil {
		return "", nil
	}
	return f.handler(argv)
}

// called reports whether any recorded call starts with prefix.
func (f *fakeHost) called(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeHost) reset() { f.calls = nil }

func useFakeHost(t *testing.T, handler func(argv []string) (string, error)) *fakeHost {
	t.Helper()
	f := &fakeHost{handler: handler}
	oldPriv, oldPlain, oldSD, oldPM, oldBP := execPriv, execPlain, systemdPresent, pkgManager, bindPaths
	execPriv = func(argv ...string) (string, error) { return f.run(argv) }
	execPlain = func(name string, args ...string) (string, error) { return f.run(append([]string{name}, args...)) }
	systemdPresent = func() bool { return true }
	pkgManager = func() PkgManager { return PkgApt }
	t.Cleanup(func() {
		execPriv, execPlain, systemdPresent, pkgManager, bindPaths = oldPriv, oldPlain, oldSD, oldPM, oldBP
	})
	return f
}

// exitErr produces a real *exec.ExitError with the given status.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatal("expected an exit error")
	}
	return err
}

// tempBind is a BIND layout entirely inside a temp dir.
func tempBind(t *testing.T) bind.Paths {
	t.Helper()
	dir := t.TempDir()
	return bind.Paths{
		Family:    bind.Debian,
		ConfDir:   dir + "/etc/bind",
		MainConf:  dir + "/etc/bind/named.conf.local",
		Include:   dir + "/etc/bind/linexus-zones.conf",
		ZonesDir:  dir + "/var/lib/bind/linexus",
		Service:   "named",
		CheckZone: "named-checkzone",
		CheckConf: "named-checkconf",
		Rndc:      "rndc",
	}
}
