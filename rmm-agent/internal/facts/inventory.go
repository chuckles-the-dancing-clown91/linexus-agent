package facts

import (
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/bind"
)

// ---- Services ----------------------------------------------------------------

// Service is one systemd service unit.
type Service struct {
	Name        string `json:"name"`
	State       string `json:"state"`  // ACTIVE column: active, inactive, failed, …
	Detail      string `json:"detail"` // SUB column: running, exited, dead, …
	Enabled     bool   `json:"enabled"`
	Description string `json:"description"`
}

// Services lists service units with their run and enable state. Only when
// systemd is the running init system; otherwise nil.
func Services() []Service {
	if !systemdRunning() {
		return nil
	}
	units, err := runTool("systemctl", "list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager")
	if err != nil && units == "" {
		return nil
	}
	files, _ := runTool("systemctl", "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	return ParseServices(units, files)
}

// ParseServices joins `systemctl list-units --plain --no-legend` output (UNIT
// LOAD ACTIVE SUB DESCRIPTION…) with `systemctl list-unit-files --no-legend`
// output (UNIT STATE [PRESET]) into services sorted by name.
func ParseServices(units, unitFiles string) []Service {
	enabled := map[string]string{}
	for _, line := range trimLines(unitFiles) {
		f := strings.Fields(line)
		if len(f) >= 2 {
			enabled[f[0]] = f[1]
		}
	}
	isEnabled := func(name string) bool {
		st, ok := enabled[name]
		if !ok {
			// An instance (foo@bar.service) inherits its template's state.
			if at := strings.Index(name, "@"); at >= 0 {
				st = enabled[name[:at+1]+".service"]
			}
		}
		return strings.HasPrefix(st, "enabled")
	}
	var out []Service
	for _, line := range trimLines(units) {
		f := strings.Fields(line)
		// Some systemd versions keep the "●" marker even with --plain.
		if len(f) > 0 && (f[0] == "●" || f[0] == "*" || f[0] == "×") {
			f = f[1:]
		}
		if len(f) < 4 || !strings.HasSuffix(f[0], ".service") {
			continue
		}
		out = append(out, Service{
			Name:        f[0],
			State:       f[2],
			Detail:      f[3],
			Enabled:     isEnabled(f[0]),
			Description: strings.Join(f[4:], " "),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---- Packages ----------------------------------------------------------------

// Package is one installed package.
type Package struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Manager string `json:"manager"` // "apt" (dpkg) or "dnf" (rpm)
}

// MaxPackages caps the package list.
const MaxPackages = 5000

// Packages lists installed packages via dpkg-query or rpm, whichever exists.
func Packages() []Package {
	if _, err := lookPath("dpkg-query"); err == nil {
		out, err := runTool("dpkg-query", "-W", "-f=${Package}\t${Version}\t${Status}\n")
		if err == nil || out != "" {
			return ParseDpkg(out)
		}
	}
	if _, err := lookPath("rpm"); err == nil {
		out, err := runTool("rpm", "-qa", "--qf", "%{NAME}\t%{VERSION}-%{RELEASE}\n")
		if err == nil || out != "" {
			return ParseRPM(out)
		}
	}
	return nil
}

// ParseDpkg parses `dpkg-query -W -f='${Package}\t${Version}\t${Status}\n'`,
// keeping only installed packages (dpkg also remembers removed ones whose
// configuration is left behind). A line without a status column is taken as
// installed.
func ParseDpkg(out string) []Package {
	var pkgs []Package
	for _, line := range trimLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		if len(f) >= 3 && !strings.HasSuffix(strings.TrimSpace(f[2]), " installed") {
			continue
		}
		pkgs = append(pkgs, Package{Name: f[0], Version: f[1], Manager: "apt"})
	}
	return capPackages(pkgs)
}

// ParseRPM parses `rpm -qa --qf '%{NAME}\t%{VERSION}-%{RELEASE}\n'`.
func ParseRPM(out string) []Package {
	var pkgs []Package
	for _, line := range trimLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] == "" || f[0] == "gpg-pubkey" {
			continue
		}
		pkgs = append(pkgs, Package{Name: f[0], Version: f[1], Manager: "dnf"})
	}
	return capPackages(pkgs)
}

func capPackages(p []Package) []Package {
	sort.SliceStable(p, func(i, j int) bool {
		if p[i].Name != p[j].Name {
			return p[i].Name < p[j].Name
		}
		return p[i].Version < p[j].Version
	})
	if len(p) > MaxPackages {
		p = p[:MaxPackages]
	}
	return p
}

// ---- DNS server --------------------------------------------------------------

// DNSServer describes a BIND server on this host.
type DNSServer struct {
	Software string   `json:"software"` // "bind9"
	Version  string   `json:"version"`
	Running  bool     `json:"running"`
	Zones    []string `json:"zones"` // the zones the agent manages
}

// bindPaths is where the agent's BIND registry lives (overridable in tests
// and by the AGENT_BIND_* environment variables).
var bindPaths = func() bind.Paths { return bind.PathsFromEnv(os.Getenv) }

// DetectDNSServer reports BIND when its binary is installed; nil otherwise.
func DetectDNSServer() *DNSServer {
	named := ""
	if p, err := lookPath("named"); err == nil {
		named = p
	} else {
		for _, c := range []string{"/usr/sbin/named", "/usr/bin/named"} {
			if _, err := os.Stat(c); err == nil {
				named = c
				break
			}
		}
	}
	if named == "" {
		return nil
	}
	d := &DNSServer{Software: "bind9", Zones: []string{}}
	if out, err := runTool(named, "-v"); err == nil || out != "" {
		d.Version = ParseNamedVersion(out)
	}
	if systemdRunning() {
		for _, unit := range []string{"named", "bind9"} {
			if _, err := runTool("systemctl", "is-active", "--quiet", unit); err == nil {
				d.Running = true
				break
			}
		}
	} else {
		d.Running = processRunning(procRoot, "named")
	}
	if reg, err := bind.LoadRegistry(bindPaths().Registry()); err == nil {
		d.Zones = reg.Names()
	}
	return d
}

var namedVersionRe = regexp.MustCompile(`\b(\d+\.\d+(?:\.\d+)?)`)

// ParseNamedVersion extracts "9.18.28" from `named -v` output such as
// "BIND 9.18.28-0ubuntu0.22.04.1-Ubuntu (Extended Support Version) <id:>".
func ParseNamedVersion(out string) string {
	if m := namedVersionRe.FindStringSubmatch(out); m != nil {
		return m[1]
	}
	return strings.TrimSpace(out)
}

// processRunning reports whether a process with this comm exists.
func processRunning(root, comm string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !isDigits(e.Name()) {
			continue
		}
		if b, err := os.ReadFile(root + "/" + e.Name() + "/comm"); err == nil &&
			strings.TrimSpace(string(b)) == comm {
			return true
		}
	}
	return false
}
