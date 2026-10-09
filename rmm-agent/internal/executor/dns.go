package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/bind"
)

// bindPaths is the BIND layout on this host (AGENT_BIND_* overrides apply).
var bindPaths = func() bind.Paths { return bind.PathsFromEnv(os.Getenv) }

// ---- dns.server.ensure -------------------------------------------------------

// ensureDNSServer is dns.server.ensure: install BIND (bind9 + bind9-utils on
// apt, bind + bind-utils on dnf), create the agent's zones directory and
// include file, add one include line to the main config, validate the
// configuration, and enable + start the service.
func ensureDNSServer(p bind.Paths) StepResult {
	res := StepResult{OK: true}
	var notes []string
	failf := func(format string, a ...any) StepResult {
		res.OK = false
		res.Err = "dns.server.ensure: " + fmt.Sprintf(format, a...)
		res.Output = strings.Join(notes, "\n")
		return res
	}

	var pkgs []string
	switch pkgManager() {
	case PkgApt:
		pkgs = []string{"bind9", "bind9-utils"}
	case PkgDnf:
		pkgs = []string{"bind", "bind-utils"}
	default:
		return failf("no supported package manager (apt/dnf) found")
	}
	for _, name := range pkgs {
		r := ensurePackage(name, "present", "")
		if !r.OK {
			return failf("install %s: %s", name, firstNonEmptyStr(r.Err, r.Output))
		}
		if r.Changed {
			res.Changed = true
		}
		notes = append(notes, r.Output)
	}

	// The zones directory: named must be able to write it (secondaries keep
	// transferred zones there).
	created, err := ensureDir(p.ZonesDir, 0o775)
	if err != nil {
		return failf("%v", err)
	}
	if created {
		res.Changed = true
		notes = append(notes, "created "+p.ZonesDir)
		if n := chownToService(p.ZonesDir, p.Owner); n != "" {
			notes = append(notes, n)
		}
		if p.Family == bind.RHEL {
			// Give the new directory named's SELinux context (best effort).
			if _, err := exec.LookPath("restorecon"); err == nil {
				_, _ = execPriv("restorecon", "-R", p.ZonesDir)
			}
		}
	}

	reg, err := bind.LoadRegistry(p.Registry())
	if err != nil {
		return failf("%v", err)
	}
	if _, err := ensureDir(filepath.Dir(p.Include), 0o755); err != nil {
		return failf("%v", err)
	}
	wrote, err := writeAtomic(p.Include, []byte(bind.RenderInclude(reg)), 0o644)
	if err != nil {
		return failf("write %s: %v", p.Include, err)
	}
	if wrote {
		res.Changed = true
		notes = append(notes, "wrote "+p.Include)
	}

	prevMain, err := os.ReadFile(p.MainConf)
	if err != nil && !os.IsNotExist(err) {
		return failf("read %s: %v", p.MainConf, err)
	}
	newMain, added := bind.WithIncludeLine(string(prevMain), p.Include)
	if added {
		if _, err := writeAtomic(p.MainConf, []byte(newMain), 0o644); err != nil {
			return failf("write %s: %v", p.MainConf, err)
		}
		res.Changed = true
		notes = append(notes, fmt.Sprintf("added %s to %s", p.IncludeLine(), p.MainConf))
	}

	// Never leave named with a configuration it cannot load: validate, and
	// take our include line back out if the result does not check.
	if out, err := execPlain(p.CheckConf); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			notes = append(notes, p.CheckConf+" not installed; configuration not validated")
		} else {
			if added {
				_, _ = writeAtomic(p.MainConf, prevMain, 0o644)
				notes = append(notes, "reverted "+p.MainConf)
			}
			return failf("%s rejected the configuration: %s", p.CheckConf, firstNonEmptyStr(out, err.Error()))
		}
	}

	svc := resolveBindService(p)
	wasActive := systemdPresent() && serviceActive(svc)
	r := ensureService(svc, "started", "true", false)
	if !r.OK {
		return failf("service %s: %s", svc, r.Err)
	}
	if r.Changed {
		res.Changed = true
	}
	notes = append(notes, fmt.Sprintf("%s: %s", svc, r.Output))

	// A running named only sees a new include after reconfig.
	if wasActive && (added || wrote) {
		if out, err := execPriv(p.Rndc, "reconfig"); err != nil {
			return failf("rndc reconfig: %s", firstNonEmptyStr(out, err.Error()))
		}
		notes = append(notes, "rndc reconfig")
	}

	res.Output = strings.Join(notes, "\n")
	return res
}

// resolveBindService picks the BIND unit: the override when set, else
// whichever of named / bind9 is a real unit file (on Debian 12+ and Ubuntu
// 22.04+ bind9.service is an alias of named.service, and enabling an alias
// fails), defaulting to bind9 on Debian and named elsewhere.
func resolveBindService(p bind.Paths) string {
	if p.Service != "" {
		return p.Service
	}
	out, _ := execPlain("systemctl", "list-unit-files", "--no-legend", "--no-pager", "named.service", "bind9.service")
	states := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			states[strings.TrimSuffix(f[0], ".service")] = f[1]
		}
	}
	for _, name := range []string{"named", "bind9"} {
		if st, ok := states[name]; ok && st != "alias" {
			return name
		}
	}
	if p.Family == bind.Debian {
		return "bind9"
	}
	return "named"
}

// ---- dns.zone.apply ----------------------------------------------------------

// applyZone is dns.zone.apply: params zone, content (the whole zone file —
// primaries only), role (primary|secondary), primaries (comma-separated IPs,
// secondaries only), secondaries (comma-separated IPs a primary allows
// transfers to and notifies), serial (informational).
//
// A primary's content is validated with named-checkzone before it replaces
// the live file (a zone that fails leaves the old file in place and fails the
// step). The zone's stanza is kept in the agent's registry and the include
// file is re-rendered from it. New or re-configured zones get `rndc reconfig`,
// an existing primary whose file changed gets `rndc reload <zone>`. An
// unchanged file and stanza is unchanged and reloads nothing.
func applyZone(p bind.Paths, params map[string]string) StepResult {
	res := StepResult{OK: true}
	failf := func(format string, a ...any) StepResult {
		res.OK = false
		res.Err = "dns.zone.apply: " + fmt.Sprintf(format, a...)
		return res
	}

	zone, err := bind.NormalizeZone(params["zone"])
	if err != nil {
		return failf("%v", err)
	}
	role := strings.ToLower(strings.TrimSpace(params["role"]))
	switch role {
	case "", "primary", "master":
		role = "primary"
	case "secondary", "slave":
		role = "secondary"
	default:
		return failf("role must be primary or secondary, not %q", params["role"])
	}
	primaries, err := bind.ParseAddrList(params["primaries"])
	if err != nil {
		return failf("primaries: %v", err)
	}
	secondaries, err := bind.ParseAddrList(params["secondaries"])
	if err != nil {
		return failf("secondaries: %v", err)
	}
	serial := strings.TrimSpace(params["serial"])
	if len(serial) > 10 || strings.Trim(serial, "0123456789") != "" {
		return failf("serial must be a number of at most 10 digits")
	}
	content := params["content"]
	if role == "primary" {
		if err := bind.CheckZoneContent(content); err != nil {
			return failf("%v", err)
		}
	} else if len(primaries) == 0 {
		return failf("a secondary needs primaries")
	}

	if err := requireIncluded(p); err != nil {
		return failf("%v", err)
	}
	if _, err := ensureDir(p.ZonesDir, 0o775); err != nil {
		return failf("%v", err)
	}
	reg, err := bind.LoadRegistry(p.Registry())
	if err != nil {
		return failf("%v", err)
	}
	old, existed := reg.Zones[zone]
	file := p.ZoneFile(zone)
	var notes []string

	fileChanged := false
	if role == "primary" {
		cur, _ := os.ReadFile(file)
		if string(cur) != content {
			if err := installZoneFile(p, zone, file, content); err != nil {
				return failf("%v", err)
			}
			fileChanged = true
			notes = append(notes, "wrote "+file)
			if n := chownToService(file, p.Owner); n != "" {
				notes = append(notes, n)
			}
		}
	}

	want := bind.Zone{Role: role, File: file, Serial: serial}
	if role == "primary" {
		want.Secondaries = secondaries
	} else {
		want.Primaries = primaries
	}
	stanzaChanged := !existed || !old.SameStanza(want)

	if !fileChanged && !stanzaChanged && !old.Pending {
		if serial != "" && serial != old.Serial {
			old.Serial = serial
			reg.Zones[zone] = old
			if _, err := writeAtomic(p.Registry(), reg.Marshal(), 0o644); err != nil {
				return failf("write registry: %v", err)
			}
			if role == "secondary" {
				// Nudge the secondary to check its primary now rather than
				// waiting for NOTIFY or the refresh timer.
				if out, err := execPriv(p.Rndc, "refresh", zone); err != nil {
					notes = append(notes, "rndc refresh failed: "+firstNonEmptyStr(out, err.Error()))
				} else {
					notes = append(notes, "rndc refresh "+zone)
				}
			}
		}
		res.Output = strings.TrimSpace(fmt.Sprintf("zone %s unchanged (%s, serial %s)\n%s",
			zone, role, orDash(old.Serial), strings.Join(notes, "\n")))
		return res
	}

	// Record the zone as pending before touching named: if the reload fails
	// the next apply retries it even though the files then look unchanged.
	want.Pending = true
	reg.Zones[zone] = want
	if err := saveZones(p, reg); err != nil {
		return failf("%v", err)
	}
	res.Changed = true
	if stanzaChanged {
		notes = append(notes, "stanza written to "+p.Include)
	}

	if !existed || stanzaChanged || old.Pending {
		if out, err := execPriv(p.Rndc, "reconfig"); err != nil {
			res.Output = strings.Join(notes, "\n")
			return failf("rndc reconfig: %s", firstNonEmptyStr(out, err.Error()))
		}
		notes = append(notes, "rndc reconfig")
	}
	if role == "primary" && existed && (fileChanged || old.Pending) {
		if out, err := execPriv(p.Rndc, "reload", zone); err != nil {
			res.Output = strings.Join(notes, "\n")
			return failf("rndc reload %s: %s", zone, firstNonEmptyStr(out, err.Error()))
		}
		notes = append(notes, "rndc reload "+zone)
	}

	want.Pending = false
	reg.Zones[zone] = want
	if err := saveZones(p, reg); err != nil {
		return failf("%v", err)
	}
	res.Output = fmt.Sprintf("zone %s applied (%s, serial %s)\n%s", zone, role, orDash(serial), strings.Join(notes, "\n"))
	return res
}

// installZoneFile validates content with named-checkzone in a temp file next
// to the live one and only then renames it into place.
func installZoneFile(p bind.Paths, zone, file, content string) error {
	tmp, err := os.CreateTemp(p.ZonesDir, ".db."+zone+".*")
	if err != nil {
		return fmt.Errorf("stage zone file: %v", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after the rename
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return fmt.Errorf("stage zone file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("stage zone file: %v", err)
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	out, err := execPlain(p.CheckZone, zone, name)
	if err != nil {
		return fmt.Errorf("%s rejected the zone (the live file is untouched): %s",
			p.CheckZone, strings.TrimSpace(strings.ReplaceAll(firstNonEmptyStr(out, err.Error()), name, "db."+zone)))
	}
	if err := os.Rename(name, file); err != nil {
		return fmt.Errorf("install zone file: %v", err)
	}
	return nil
}

// requireIncluded refuses to manage zones on a host whose named does not load
// the agent's include file — reconfig would "succeed" and serve nothing.
func requireIncluded(p bind.Paths) error {
	conf, err := os.ReadFile(p.MainConf)
	if err != nil || !bind.HasIncludeLine(string(conf), p.Include) {
		return fmt.Errorf("BIND is not set up for Linexus zones (%s does not include %s): run install_dns_server first",
			p.MainConf, p.Include)
	}
	return nil
}

// saveZones writes the registry and re-renders the include file from it.
func saveZones(p bind.Paths, reg bind.Registry) error {
	if _, err := writeAtomic(p.Registry(), reg.Marshal(), 0o644); err != nil {
		return fmt.Errorf("write registry: %v", err)
	}
	if _, err := writeAtomic(p.Include, []byte(bind.RenderInclude(reg)), 0o644); err != nil {
		return fmt.Errorf("write %s: %v", p.Include, err)
	}
	return nil
}

// ---- dns.zone.remove ---------------------------------------------------------

// removeZone is dns.zone.remove: drop the zone's stanza, `rndc reconfig`, then
// delete its file (and journal). Removing an absent zone is unchanged.
func removeZone(p bind.Paths, rawZone string) StepResult {
	res := StepResult{OK: true}
	zone, err := bind.NormalizeZone(rawZone)
	if err != nil {
		return StepResult{Err: "dns.zone.remove: " + err.Error()}
	}
	reg, err := bind.LoadRegistry(p.Registry())
	if err != nil {
		return StepResult{Err: "dns.zone.remove: " + err.Error()}
	}
	file := p.ZoneFile(zone)
	_, existed := reg.Zones[zone]
	hadFile := fileExists(file)
	if !existed && !hadFile {
		res.Output = fmt.Sprintf("zone %s not present", zone)
		return res
	}
	delete(reg.Zones, zone)
	if err := saveZones(p, reg); err != nil {
		return StepResult{Err: "dns.zone.remove: " + err.Error()}
	}
	res.Changed = true
	notes := []string{"stanza removed from " + p.Include}
	if existed {
		if out, err := execPriv(p.Rndc, "reconfig"); err != nil {
			return StepResult{Changed: true, Output: strings.Join(notes, "\n"),
				Err: "dns.zone.remove: rndc reconfig: " + firstNonEmptyStr(out, err.Error())}
		}
		notes = append(notes, "rndc reconfig")
	}
	for _, f := range []string{file, file + ".jnl", file + ".jbk"} {
		if err := os.Remove(f); err == nil {
			notes = append(notes, "removed "+f)
		} else if !os.IsNotExist(err) {
			notes = append(notes, fmt.Sprintf("remove %s: %v", f, err))
		}
	}
	res.Output = fmt.Sprintf("zone %s removed\n%s", zone, strings.Join(notes, "\n"))
	return res
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
