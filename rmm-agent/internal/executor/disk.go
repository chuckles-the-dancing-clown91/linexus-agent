package executor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Seams for disk.mount tests.
var (
	fstabPath        = "/etc/fstab"
	mountsPath       = "/proc/self/mounts"
	deviceWait       = 60 * time.Second
	devicePollPeriod = time.Second
)

// Mount points disk.mount refuses: mounting a volume over any of these would
// hide the running system.
var protectedMountPoints = map[string]bool{
	"/": true, "/bin": true, "/boot": true, "/dev": true, "/etc": true, "/lib": true,
	"/lib32": true, "/lib64": true, "/proc": true, "/root": true, "/run": true,
	"/sbin": true, "/sys": true, "/usr": true, "/var": true, "/var/lib": true,
	"/var/lib/linexus": true, "/tmp": true,
}

// mountDisk is disk.mount: params device (a /dev path, typically
// /dev/disk/by-id/scsi-0DO_Volume_<name>), mountPoint, fsType (ext4|xfs,
// default ext4), format (if_blank|never, default if_blank).
//
// It waits up to 60 s for the device, probes it with blkid, and creates a
// filesystem only when the device carries no signature at all (no
// filesystem, no partition table, no RAID/LVM member) and format=if_blank.
// It never formats a device that has one. It then makes the mount point,
// adds `UUID=<uuid> <mountPoint> <fs> defaults,nofail,discard 0 2` to fstab
// once, and mounts. Everything already in place is unchanged.
func mountDisk(p map[string]string) StepResult {
	res := StepResult{OK: true}
	var notes []string
	failf := func(format string, a ...any) StepResult {
		res.OK = false
		res.Err = "disk.mount: " + fmt.Sprintf(format, a...)
		res.Output = strings.Join(notes, "\n")
		return res
	}

	device := strings.TrimSpace(p["device"])
	mp := strings.TrimSpace(p["mountPoint"])
	fsType := strings.ToLower(strings.TrimSpace(p["fsType"]))
	if fsType == "" {
		fsType = "ext4"
	}
	format := strings.ToLower(strings.TrimSpace(p["format"]))
	if format == "" {
		format = "if_blank"
	}
	if err := validateDevice(device); err != nil {
		return failf("%v", err)
	}
	if err := validateMountPoint(mp); err != nil {
		return failf("%v", err)
	}
	if fsType != "ext4" && fsType != "xfs" {
		return failf("fsType must be ext4 or xfs, not %q", fsType)
	}
	if format != "if_blank" && format != "never" {
		return failf("format must be if_blank or never, not %q", format)
	}

	if err := waitForDevice(device); err != nil {
		return failf("%v", err)
	}
	realDev, err := filepath.EvalSymlinks(device)
	if err != nil {
		realDev = device
	}

	probe, err := probeDevice(device)
	if err != nil {
		return failf("%v", err)
	}
	actualFS := probe["TYPE"]
	switch {
	case actualFS == "" && len(probe) > 0:
		// Something is on it — a partition table, or a signature blkid
		// knows but that is not a filesystem. Not blank: never format.
		return failf("%s is not blank (%s) and carries no filesystem to mount; refusing to touch it", device, describeProbe(probe))
	case actualFS == "" && format == "never":
		return failf("%s has no filesystem and format=never", device)
	case actualFS == "":
		argv := []string{"mkfs.ext4", "-q", "-F", device}
		if fsType == "xfs" {
			// No -f: mkfs.xfs then refuses on any signature it finds itself.
			argv = []string{"mkfs.xfs", "-q", device}
		}
		if out, err := execPriv(argv...); err != nil {
			return failf("%s: %s", argv[0], firstNonEmptyStr(out, err.Error()))
		}
		res.Changed = true
		notes = append(notes, fmt.Sprintf("created %s on blank %s", fsType, device))
		if probe, err = probeDevice(device); err != nil {
			return failf("%v", err)
		}
		actualFS = probe["TYPE"]
	case actualFS != fsType:
		notes = append(notes, fmt.Sprintf("%s already carries %s (asked for %s); mounting it as %s, not reformatting",
			device, actualFS, fsType, actualFS))
	default:
		notes = append(notes, fmt.Sprintf("%s already carries %s", device, actualFS))
	}
	if actualFS == "" {
		return failf("%s has no filesystem after mkfs", device)
	}
	uuid := probe["UUID"]
	if uuid == "" || strings.ContainsAny(uuid, " \t\n") {
		return failf("%s has no usable filesystem UUID", device)
	}

	created, err := ensureDir(mp, 0o755)
	if err != nil {
		return failf("%v", err)
	}
	if created {
		res.Changed = true
		notes = append(notes, "created "+mp)
	}

	cur, err := os.ReadFile(fstabPath)
	if err != nil && !os.IsNotExist(err) {
		return failf("read %s: %v", fstabPath, err)
	}
	next, added, err := ensureFstabEntry(string(cur), uuid, mp, actualFS, []string{device, realDev})
	if err != nil {
		return failf("%v", err)
	}
	if added {
		backup := fstabPath + ".linexus.bak"
		if len(cur) > 0 && !fileExists(backup) {
			_, _ = writeAtomic(backup, cur, 0o644)
		}
		if _, err := writeAtomic(fstabPath, []byte(next), 0o644); err != nil {
			return failf("write %s: %v", fstabPath, err)
		}
		res.Changed = true
		notes = append(notes, "added to "+fstabPath)
		if systemdPresent() {
			_, _ = execPriv("systemctl", "daemon-reload")
		}
	}

	mounts, err := os.ReadFile(mountsPath)
	if err != nil {
		return failf("read %s: %v", mountsPath, err)
	}
	if src, ok := mountedAt(string(mounts), mp); ok {
		srcReal, err := filepath.EvalSymlinks(src)
		if err != nil {
			srcReal = src
		}
		if srcReal != realDev && src != device {
			return failf("%s is already mounted from %s", mp, src)
		}
		notes = append(notes, fmt.Sprintf("%s already mounted on %s", device, mp))
	} else {
		if out, err := execPriv("mount", mp); err != nil {
			return failf("mount %s: %s", mp, firstNonEmptyStr(out, err.Error()))
		}
		res.Changed = true
		notes = append(notes, fmt.Sprintf("mounted %s on %s", device, mp))
	}

	res.Output = strings.Join(notes, "\n")
	return res
}

func validateDevice(d string) error {
	if d == "" {
		return fmt.Errorf("missing 'device'")
	}
	if !strings.HasPrefix(d, "/dev/") || filepath.Clean(d) != d || strings.ContainsAny(d, " \t\n\\\"'") {
		return fmt.Errorf("device must be a clean /dev/ path, not %q", d)
	}
	return nil
}

func validateMountPoint(mp string) error {
	if mp == "" {
		return fmt.Errorf("missing 'mountPoint'")
	}
	if !filepath.IsAbs(mp) || filepath.Clean(mp) != mp || strings.ContainsAny(mp, " \t\n\\\"'#") {
		return fmt.Errorf("mountPoint must be a clean absolute path without spaces, not %q", mp)
	}
	if protectedMountPoints[mp] || strings.HasPrefix(mp, "/proc/") || strings.HasPrefix(mp, "/sys/") ||
		strings.HasPrefix(mp, "/dev/") || strings.HasPrefix(mp, "/boot/") || strings.HasPrefix(mp, "/run/") {
		return fmt.Errorf("refusing to mount over %s", mp)
	}
	return nil
}

func waitForDevice(device string) error {
	deadline := time.Now().Add(deviceWait)
	for {
		if _, err := os.Stat(device); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("device %s did not appear within %s", device, deviceWait)
		}
		time.Sleep(devicePollPeriod)
	}
}

// probeDevice runs a low-level blkid probe (which also sees partition tables)
// and returns its KEY=VALUE pairs. An empty map means blkid found nothing:
// the device is blank.
func probeDevice(device string) (map[string]string, error) {
	out, err := execPriv("blkid", "-p", "-o", "export", device)
	if err != nil {
		if exitCodeOf(err) == 2 && strings.TrimSpace(out) == "" {
			return map[string]string{}, nil // nothing found
		}
		return nil, fmt.Errorf("blkid %s: %s", device, firstNonEmptyStr(out, err.Error()))
	}
	return parseBlkidExport(out), nil
}

func parseBlkidExport(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k != "" && k != "DEVNAME" {
			m[k] = v
		}
	}
	return m
}

func describeProbe(m map[string]string) string {
	var parts []string
	for _, k := range []string{"PTTYPE", "TYPE", "USAGE", "PART_ENTRY_TYPE"} {
		if v := m[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) == 0 {
		return "unknown signature"
	}
	return strings.Join(parts, " ")
}

// ensureFstabEntry returns fstab content with the volume's line present.
// It is unchanged when a line already mounts this UUID (or one of the
// device's other names) at mp; it refuses when mp is already claimed by a
// different source, or this UUID is already mounted elsewhere.
func ensureFstabEntry(content, uuid, mp, fsType string, aliases []string) (string, bool, error) {
	ours := map[string]bool{"UUID=" + uuid: true, "/dev/disk/by-uuid/" + uuid: true}
	for _, a := range aliases {
		if a != "" {
			ours[a] = true
		}
	}
	sc := bufio.NewScanner(strings.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		spec, file := unquoteSpec(f[0]), unescapeMount(f[1])
		switch {
		case ours[spec] && file == mp:
			return content, false, nil
		case ours[spec]:
			return "", false, fmt.Errorf("this volume is already in fstab at %s", file)
		case file == mp:
			return "", false, fmt.Errorf("%s is already in fstab for %s", mp, f[0])
		}
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += fmt.Sprintf("UUID=%s %s %s defaults,nofail,discard 0 2\n", uuid, mp, fsType)
	return content, true, nil
}

func unquoteSpec(s string) string {
	if k, v, ok := strings.Cut(s, "="); ok {
		return k + "=" + strings.Trim(v, `"`)
	}
	return s
}

// mountedAt finds the source mounted at mp in /proc/self/mounts content (the
// last one wins: later mounts shadow earlier ones).
func mountedAt(mounts, mp string) (string, bool) {
	src, found := "", false
	for _, line := range strings.Split(mounts, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && unescapeMount(f[1]) == mp {
			src, found = unescapeMount(f[0]), true
		}
	}
	return src, found
}

// unescapeMount decodes the octal escapes (\040 for a space, …) used in
// fstab and /proc/self/mounts.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
