package executor

import (
	"fmt"
	"os/exec"
	"strings"
)

// packageInstalled reports whether a package is installed and, for apt, its
// version. This is the native-first state read that makes package.ensure
// idempotent — no mutation happens if the desired state already holds.
func packageInstalled(pm PkgManager, name string) (bool, string) {
	switch pm {
	case PkgApt:
		out, err := exec.Command("dpkg-query", "-W", "-f=${Version}", name).Output()
		if err != nil {
			return false, ""
		}
		v := strings.TrimSpace(string(out))
		return v != "", v
	case PkgDnf:
		err := exec.Command("rpm", "-q", name).Run()
		return err == nil, ""
	default:
		return false, ""
	}
}

func ensurePackage(name, state, version string) StepResult {
	res := StepResult{OK: true}
	if name == "" {
		res.OK = false
		res.Err = "package.ensure: missing 'name'"
		return res
	}
	pm := detectPkgManager()
	if pm == PkgUnknown {
		res.OK = false
		res.Err = "package.ensure: no supported package manager (apt/dnf) found"
		return res
	}

	installed, curVer := packageInstalled(pm, name)
	desiredPresent := state != "absent" // default is present

	if desiredPresent {
		if installed && (version == "" || version == curVer) {
			res.Output = fmt.Sprintf("package %q already present (%s)", name, orNone(curVer))
			return res // unchanged
		}
		out, err := installPackage(pm, name, version)
		res.Changed = true
		res.Output = out
		if err != nil {
			res.OK = false
			res.Err = err.Error()
		} else {
			res.Output = fmt.Sprintf("installed %q", name)
		}
		return res
	}

	// desired absent
	if !installed {
		res.Output = fmt.Sprintf("package %q already absent", name)
		return res // unchanged
	}
	out, err := removePackage(pm, name)
	res.Changed = true
	res.Output = out
	if err != nil {
		res.OK = false
		res.Err = err.Error()
	} else {
		res.Output = fmt.Sprintf("removed %q", name)
	}
	return res
}

func installPackage(pm PkgManager, name, version string) (string, error) {
	switch pm {
	case PkgApt:
		pkg := name
		if version != "" {
			pkg = name + "=" + version
		}
		return runPrivileged("apt-get", "install", "-y", pkg)
	case PkgDnf:
		pkg := name
		if version != "" {
			pkg = name + "-" + version
		}
		return runPrivileged("dnf", "install", "-y", pkg)
	default:
		return "", fmt.Errorf("unsupported package manager")
	}
}

func removePackage(pm PkgManager, name string) (string, error) {
	switch pm {
	case PkgApt:
		return runPrivileged("apt-get", "remove", "-y", name)
	case PkgDnf:
		return runPrivileged("dnf", "remove", "-y", name)
	default:
		return "", fmt.Errorf("unsupported package manager")
	}
}

func orNone(s string) string {
	if s == "" {
		return "installed"
	}
	return s
}
