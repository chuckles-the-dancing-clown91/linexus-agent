// Package bind holds what the agent knows about the BIND server it manages:
// where its files live on this distribution, the small JSON registry of the
// zones the agent owns, how that registry renders into the include file named
// loads, and the validation applied to zone names and addresses that arrive
// from the network.
//
// The agent owns exactly two things inside BIND's configuration: one include
// file (linexus-zones.conf) and one zones directory. Everything else in the
// operator's named configuration is left alone; the main config only gains a
// single `include "…/linexus-zones.conf";` line.
package bind

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Family is the distribution family, which decides BIND's file layout.
type Family string

const (
	Debian Family = "debian"
	RHEL   Family = "rhel"
)

// Paths is where the agent's BIND files live. Every field can be overridden
// from the environment so the executors are testable without root and so an
// unusual layout can be accommodated.
type Paths struct {
	Family Family
	// ConfDir is BIND's configuration directory (/etc/bind, /etc/named).
	ConfDir string
	// MainConf is the file that gains the include line (named.conf.local on
	// Debian, /etc/named.conf on RHEL).
	MainConf string
	// Include is the agent-managed include file listing its zones.
	Include string
	// ZonesDir holds db.<zone> files and the registry.
	ZonesDir string
	// Service is the systemd unit; empty means "detect" (named or bind9).
	Service string
	// CheckZone, CheckConf and Rndc are the commands used to validate and
	// reload.
	CheckZone string
	CheckConf string
	Rndc      string
	// Owner is the account named runs as (bind or named); zone directories
	// are handed to it so secondaries can write transferred zones.
	Owner string
}

// Registry is the path of the zone registry inside ZonesDir.
func (p Paths) Registry() string { return filepath.Join(p.ZonesDir, "linexus-zones.json") }

// ZoneFile is where a zone's data lives.
func (p Paths) ZoneFile(zone string) string { return filepath.Join(p.ZonesDir, "db."+zone) }

// IncludeLine is the line the main config must contain.
func (p Paths) IncludeLine() string { return fmt.Sprintf("include %q;", p.Include) }

// DetectFamily guesses the distribution family from the filesystem.
func DetectFamily() Family {
	if _, err := os.Stat("/etc/debian_version"); err == nil {
		return Debian
	}
	if _, err := os.Stat("/etc/redhat-release"); err == nil {
		return RHEL
	}
	if _, err := os.Stat("/etc/bind"); err == nil {
		return Debian
	}
	return RHEL
}

// DefaultPaths is the stock layout for a family.
func DefaultPaths(f Family) Paths {
	if f == Debian {
		// /var/lib/bind is where the bind9 AppArmor profile lets named write,
		// which a secondary needs for transferred zones.
		return Paths{
			Family:    Debian,
			ConfDir:   "/etc/bind",
			MainConf:  "/etc/bind/named.conf.local",
			Include:   "/etc/bind/linexus-zones.conf",
			ZonesDir:  "/var/lib/bind/linexus",
			CheckZone: "named-checkzone",
			CheckConf: "named-checkconf",
			Rndc:      "rndc",
			Owner:     "bind",
		}
	}
	return Paths{
		Family:    RHEL,
		ConfDir:   "/etc/named",
		MainConf:  "/etc/named.conf",
		Include:   "/etc/named/linexus-zones.conf",
		ZonesDir:  "/var/named/linexus",
		Service:   "named",
		CheckZone: "named-checkzone",
		CheckConf: "named-checkconf",
		Rndc:      "rndc",
		Owner:     "named",
	}
}

// PathsFromEnv is DefaultPaths(DetectFamily()) with the AGENT_BIND_*
// overrides applied: AGENT_BIND_CONF_DIR (also moves MainConf and Include
// under it unless they are set too), AGENT_BIND_MAIN_CONF,
// AGENT_BIND_INCLUDE, AGENT_BIND_ZONES_DIR, AGENT_BIND_SERVICE,
// AGENT_BIND_CHECKZONE, AGENT_BIND_CHECKCONF, AGENT_BIND_RNDC,
// AGENT_BIND_OWNER.
func PathsFromEnv(getenv func(string) string) Paths {
	p := DefaultPaths(DetectFamily())
	if v := getenv("AGENT_BIND_CONF_DIR"); v != "" {
		p.ConfDir = v
		if p.Family == Debian {
			p.MainConf = filepath.Join(v, "named.conf.local")
		} else {
			p.MainConf = filepath.Join(v, "named.conf")
		}
		p.Include = filepath.Join(v, "linexus-zones.conf")
	}
	set := func(dst *string, key string) {
		if v := getenv(key); v != "" {
			*dst = v
		}
	}
	set(&p.MainConf, "AGENT_BIND_MAIN_CONF")
	set(&p.Include, "AGENT_BIND_INCLUDE")
	set(&p.ZonesDir, "AGENT_BIND_ZONES_DIR")
	set(&p.Service, "AGENT_BIND_SERVICE")
	set(&p.CheckZone, "AGENT_BIND_CHECKZONE")
	set(&p.CheckConf, "AGENT_BIND_CHECKCONF")
	set(&p.Rndc, "AGENT_BIND_RNDC")
	set(&p.Owner, "AGENT_BIND_OWNER")
	return p
}

// ---- Validation ----------------------------------------------------------

// NormalizeZone lower-cases a zone name, drops one trailing dot, and checks
// it strictly: dot-separated labels of letters, digits and hyphens, each
// 1–63 characters, not starting or ending with a hyphen, at most 253
// characters in all. Zone names arrive from the network and become file
// names and config text, so nothing else is allowed — no slashes, no "..",
// no quotes, no whitespace.
func NormalizeZone(name string) (string, error) {
	z := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if z == "" {
		return "", fmt.Errorf("zone name is empty")
	}
	if len(z) > 253 {
		return "", fmt.Errorf("zone name is longer than 253 characters")
	}
	for _, label := range strings.Split(z, ".") {
		if label == "" {
			return "", fmt.Errorf("zone name %q has an empty label", name)
		}
		if len(label) > 63 {
			return "", fmt.Errorf("zone name %q has a label longer than 63 characters", name)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("zone name %q has a label starting or ending with '-'", name)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return "", fmt.Errorf("zone name %q may contain only letters, digits, hyphens and dots", name)
			}
		}
	}
	return z, nil
}

// ParseAddrList parses a comma- (or space- / semicolon-) separated list of IP
// addresses, rejecting anything that is not one. The result is in input
// order, de-duplicated, in canonical form.
func ParseAddrList(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' })
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		ip := net.ParseIP(f)
		if ip == nil {
			return nil, fmt.Errorf("%q is not an IP address", f)
		}
		c := ip.String()
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// CheckZoneContent refuses zone text the agent must never load: NUL bytes,
// oversized files, and $INCLUDE directives (which would make named — and
// named-checkzone — read an arbitrary file on this host).
func CheckZoneContent(content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("zone content is empty")
	}
	if len(content) > MaxZoneBytes {
		return fmt.Errorf("zone content is %d bytes, more than %d", len(content), MaxZoneBytes)
	}
	if strings.ContainsRune(content, 0) {
		return fmt.Errorf("zone content contains a NUL byte")
	}
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.EqualFold(f[0], "$INCLUDE") {
			return fmt.Errorf("zone content may not use $INCLUDE")
		}
	}
	return nil
}

// MaxZoneBytes caps one zone file.
const MaxZoneBytes = 8 << 20

// ---- Registry -------------------------------------------------------------

// Zone is one zone the agent serves.
type Zone struct {
	// Role is "primary" or "secondary".
	Role string `json:"role"`
	// File is the zone's data file.
	File string `json:"file"`
	// Primaries are the addresses a secondary transfers from.
	Primaries []string `json:"primaries,omitempty"`
	// Secondaries are the addresses a primary allows transfers to and
	// notifies.
	Secondaries []string `json:"secondaries,omitempty"`
	// Serial is the SOA serial of the last apply (informational).
	Serial string `json:"serial,omitempty"`
	// Pending marks a zone whose files were written but whose reload has not
	// succeeded yet, so the next apply retries the reload even when nothing
	// else changed.
	Pending bool `json:"pending,omitempty"`
}

// SameStanza reports whether two zones render to the same config stanza.
func (z Zone) SameStanza(o Zone) bool {
	return z.Role == o.Role && z.File == o.File &&
		strings.Join(z.Primaries, ",") == strings.Join(o.Primaries, ",") &&
		strings.Join(z.Secondaries, ",") == strings.Join(o.Secondaries, ",")
}

// Registry is the agent's record of the zones it manages; the include file is
// always re-rendered from it, so adding and removing zones stays clean.
type Registry struct {
	Zones map[string]Zone `json:"zones"`
}

// LoadRegistry reads the registry; a missing file is an empty registry.
func LoadRegistry(path string) (Registry, error) {
	r := Registry{Zones: map[string]Zone{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("registry %s: %w", path, err)
	}
	if r.Zones == nil {
		r.Zones = map[string]Zone{}
	}
	return r, nil
}

// Marshal is the registry's file content (stable: keys are sorted by
// encoding/json).
func (r Registry) Marshal() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return append(b, '\n')
}

// Names lists the zones, sorted.
func (r Registry) Names() []string {
	names := make([]string, 0, len(r.Zones))
	for n := range r.Zones {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RenderInclude renders the include file from the registry, deterministically
// (zones sorted by name).
func RenderInclude(r Registry) string {
	var b strings.Builder
	b.WriteString("// Managed by the Linexus agent — do not edit; changes are overwritten.\n")
	b.WriteString("// Source of truth: Nexus. Local registry: linexus-zones.json in the zones directory.\n")
	for _, name := range r.Names() {
		z := r.Zones[name]
		b.WriteString("\n")
		b.WriteString(RenderStanza(name, z))
	}
	return b.String()
}

// RenderStanza renders one zone statement.
func RenderStanza(name string, z Zone) string {
	var b strings.Builder
	fmt.Fprintf(&b, "zone %q {\n", name)
	if z.Role == "secondary" {
		b.WriteString("    type slave;\n")
		fmt.Fprintf(&b, "    masters { %s };\n", addrList(z.Primaries))
		fmt.Fprintf(&b, "    file %q;\n", z.File)
	} else {
		b.WriteString("    type master;\n")
		fmt.Fprintf(&b, "    file %q;\n", z.File)
		if len(z.Secondaries) > 0 {
			fmt.Fprintf(&b, "    allow-transfer { %s };\n", addrList(z.Secondaries))
			fmt.Fprintf(&b, "    also-notify { %s };\n", addrList(z.Secondaries))
		} else {
			b.WriteString("    allow-transfer { none; };\n")
		}
	}
	b.WriteString("};\n")
	return b.String()
}

func addrList(addrs []string) string {
	var b strings.Builder
	for _, a := range addrs {
		b.WriteString(a)
		b.WriteString("; ")
	}
	return strings.TrimSpace(b.String())
}

// HasIncludeLine reports whether conf already includes path (an `include`
// statement naming it, outside a comment).
func HasIncludeLine(conf, include string) bool {
	want := fmt.Sprintf("%q", include)
	for _, line := range strings.Split(conf, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "//") || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "/*") {
			continue
		}
		if strings.HasPrefix(l, "include") && strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// WithIncludeLine returns conf with the include line appended when missing.
func WithIncludeLine(conf, include string) (string, bool) {
	if HasIncludeLine(conf, include) {
		return conf, false
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	conf += fmt.Sprintf("\n// Zones managed by the Linexus agent.\ninclude %q;\n", include)
	return conf, true
}
