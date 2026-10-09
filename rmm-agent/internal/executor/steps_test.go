package executor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/bind"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/state"
)

func run(action string, params map[string]string, opts Options) StepResult {
	return ExecuteStep(nexus.Step{ID: "s", Action: action, Params: params}, opts)
}

// ---- agent.environment / agent.facts ------------------------------------------

func TestAgentEnvironmentPersistsAndIsIdempotent(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "state.json")
	if err := state.Save(sf, state.State{AgentID: "a1", AgentToken: "nxa_x"}); err != nil {
		t.Fatal(err)
	}
	opts := Options{StateFile: sf}
	params := map[string]string{"environment": "development", "monitored": "false", "note": "lab rebuild"}

	r := run("agent.environment", params, opts)
	if !r.OK || !r.Changed {
		t.Fatalf("first apply: %+v", r)
	}
	s, _ := state.Load(sf)
	if s.AgentID != "a1" || s.AgentToken != "nxa_x" {
		t.Errorf("identity lost: %+v", s)
	}
	if s.Environment == nil || s.Environment.Environment != "development" || s.Environment.Monitored ||
		s.Environment.Note != "lab rebuild" || s.Monitored() {
		t.Errorf("environment = %+v", s.Environment)
	}

	r = run("agent.environment", params, opts)
	if !r.OK || r.Changed {
		t.Fatalf("repeat should be unchanged: %+v", r)
	}

	r = run("agent.environment", map[string]string{"environment": "development", "monitored": "true", "note": "lab rebuild"}, opts)
	if !r.OK || !r.Changed {
		t.Fatalf("unmute: %+v", r)
	}
	s, _ = state.Load(sf)
	if !s.Monitored() {
		t.Error("unmute did not persist")
	}
}

func TestAgentEnvironmentDefaultsAndGarbage(t *testing.T) {
	sf := filepath.Join(t.TempDir(), "state.json")
	r := run("agent.environment", map[string]string{"monitored": "maybe"}, Options{StateFile: sf})
	if !r.OK {
		t.Fatal(r.Err)
	}
	s, _ := state.Load(sf)
	if s.Environment.Environment != "production" || !s.Environment.Monitored {
		t.Errorf("got %+v", s.Environment)
	}
	if r := run("agent.environment", nil, Options{}); r.OK {
		t.Error("no state file should fail")
	}
}

func TestAgentFactsCallsBack(t *testing.T) {
	called := 0
	r := run("agent.facts", nil, Options{SendFacts: func() (string, error) { called++; return "reported", nil }})
	if !r.OK || called != 1 || r.Output != "reported" || r.Changed {
		t.Fatalf("got %+v called=%d", r, called)
	}
	r = run("agent.facts", nil, Options{SendFacts: func() (string, error) { return "", errors.New("nexus down") }})
	if r.OK || !strings.Contains(r.Err, "nexus down") {
		t.Fatalf("got %+v", r)
	}
	if r := run("agent.facts", nil, Options{}); r.OK {
		t.Error("no callback should fail")
	}
}

// ---- service.ensure -----------------------------------------------------------

func TestServiceEnsureRestart(t *testing.T) {
	f := useFakeHost(t, nil)
	r := run("service.ensure", map[string]string{"name": "nginx", "restart": "true"}, Options{})
	if !r.OK || !r.Changed || !f.called("systemctl restart nginx") {
		t.Fatalf("got %+v calls=%v", r, f.calls)
	}
	r = run("service.ensure", map[string]string{"service": "nginx", "restart": "true", "state": "stopped"}, Options{})
	if r.OK {
		t.Error("restart + stopped should conflict")
	}
	r = run("service.ensure", map[string]string{"name": "--now; rm -rf /"}, Options{})
	if r.OK {
		t.Error("an option-looking unit name must be refused")
	}
}

func TestServiceEnsureStartStop(t *testing.T) {
	active := false
	f := useFakeHost(t, func(argv []string) (string, error) {
		if argv[0] == "systemctl" && argv[1] == "is-active" {
			if active {
				return "", nil
			}
			return "", errors.New("inactive")
		}
		return "", nil
	})
	r := run("service.ensure", map[string]string{"service": "nginx", "state": "started"}, Options{})
	if !r.OK || !r.Changed || !f.called("systemctl start nginx") {
		t.Fatalf("start: %+v %v", r, f.calls)
	}
	active = true
	f.reset()
	r = run("service.ensure", map[string]string{"name": "nginx", "state": "started"}, Options{})
	if !r.OK || r.Changed || f.called("systemctl start") {
		t.Fatalf("already started: %+v %v", r, f.calls)
	}
	r = run("service.ensure", map[string]string{"name": "nginx", "state": "stopped"}, Options{})
	if !r.OK || !r.Changed || !f.called("systemctl stop nginx") {
		t.Fatalf("stop: %+v %v", r, f.calls)
	}
}

// ---- dns.server.ensure ----------------------------------------------------------

func bindHost(t *testing.T, p bind.Paths, h func(argv []string) (string, error)) *fakeHost {
	t.Helper()
	f := useFakeHost(t, func(argv []string) (string, error) {
		if h != nil {
			if out, err, ok := callH(h, argv); ok {
				return out, err
			}
		}
		switch {
		case argv[0] == "dpkg-query":
			return "install ok installed\t1:9.18.28-1", nil
		case argv[0] == "systemctl" && (argv[1] == "is-active" || argv[1] == "is-enabled"):
			return "", nil
		}
		return "", nil
	})
	bindPaths = func() bind.Paths { return p }
	return f
}

// errPass lets a test handler decline a call so the default applies.
var errPass = errors.New("pass")

func callH(h func([]string) (string, error), argv []string) (string, error, bool) {
	out, err := h(argv)
	if errors.Is(err, errPass) {
		return "", nil, false
	}
	return out, err, true
}

func TestDNSServerEnsure(t *testing.T) {
	p := tempBind(t)
	if err := os.MkdirAll(p.ConfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orig := "//\n// Do any local configuration here\n//\n"
	if err := os.WriteFile(p.MainConf, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	f := bindHost(t, p, nil)

	r := run("dns.server.ensure", nil, Options{})
	if !r.OK || !r.Changed {
		t.Fatalf("first: %+v", r)
	}
	main, _ := os.ReadFile(p.MainConf)
	if !strings.HasPrefix(string(main), orig) || !bind.HasIncludeLine(string(main), p.Include) {
		t.Errorf("main conf = %q", main)
	}
	if st, err := os.Stat(p.ZonesDir); err != nil || !st.IsDir() {
		t.Error("zones dir not created")
	}
	inc, err := os.ReadFile(p.Include)
	if err != nil || strings.Contains(string(inc), "zone ") {
		t.Errorf("include = %q %v", inc, err)
	}
	if f.called("apt-get install") {
		t.Error("installed packages were reinstalled")
	}
	if !f.called("named-checkconf") || !f.called("rndc reconfig") {
		t.Errorf("calls = %v", f.calls)
	}

	f.reset()
	r = run("dns.server.ensure", nil, Options{})
	if !r.OK || r.Changed || f.called("rndc") {
		t.Fatalf("second run should be unchanged: %+v %v", r, f.calls)
	}
	main2, _ := os.ReadFile(p.MainConf)
	if string(main2) != string(main) {
		t.Error("include line added twice")
	}
}

func TestDNSServerEnsureInstallsAndStarts(t *testing.T) {
	p := tempBind(t)
	p.Service = ""
	installed := map[string]bool{}
	f := bindHost(t, p, func(argv []string) (string, error) {
		switch {
		case argv[0] == "dpkg-query":
			if installed[argv[len(argv)-1]] {
				return "install ok installed\t1", nil
			}
			return "", exitErrPlain()
		case argv[0] == "apt-get" && argv[1] == "install":
			installed[argv[len(argv)-1]] = true
			return "ok", nil
		case argv[0] == "systemctl" && argv[1] == "list-unit-files":
			return "named.service enabled enabled\nbind9.service alias -\n", nil
		case argv[0] == "systemctl" && (argv[1] == "is-active" || argv[1] == "is-enabled"):
			return "", errors.New("no")
		}
		return "", errPass
	})
	r := run("dns.server.ensure", nil, Options{})
	if !r.OK || !r.Changed {
		t.Fatalf("got %+v", r)
	}
	for _, want := range []string{"apt-get install -y bind9", "apt-get install -y bind9-utils", "systemctl start named", "systemctl enable named"} {
		if !f.called(want) {
			t.Errorf("missing %q in %v", want, f.calls)
		}
	}
	if f.called("rndc") {
		t.Error("rndc on a service that was not running")
	}
}

func exitErrPlain() error { return errors.New("exit status 1") }

func TestDNSServerEnsureRevertsOnBadConfig(t *testing.T) {
	p := tempBind(t)
	_ = os.MkdirAll(p.ConfDir, 0o755)
	orig := "// local\n"
	_ = os.WriteFile(p.MainConf, []byte(orig), 0o644)
	bindHost(t, p, func(argv []string) (string, error) {
		if argv[0] == "named-checkconf" {
			return "bad things", errors.New("exit status 1")
		}
		return "", errPass
	})
	r := run("dns.server.ensure", nil, Options{})
	if r.OK || !strings.Contains(r.Err, "rejected") {
		t.Fatalf("got %+v", r)
	}
	main, _ := os.ReadFile(p.MainConf)
	if string(main) != orig {
		t.Errorf("main conf not reverted: %q", main)
	}
}

// ---- dns.zone.apply / remove -----------------------------------------------------

const zoneV1 = `$TTL 3600
@ IN SOA ns1.example.com. hostmaster.example.com. 2026100901 3600 600 1209600 3600
@ IN NS ns1.example.com.
ns1 IN A 203.0.113.7
`

func readyBind(t *testing.T) bind.Paths {
	t.Helper()
	p := tempBind(t)
	_ = os.MkdirAll(p.ConfDir, 0o755)
	if err := os.WriteFile(p.MainConf, []byte(`include "`+p.Include+`";`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestZoneApplyPrimaryLifecycle(t *testing.T) {
	p := readyBind(t)
	f := bindHost(t, p, nil)
	params := map[string]string{"zone": "Example.com.", "content": zoneV1, "role": "primary",
		"secondaries": "10.0.0.9, 10.0.0.10", "serial": "2026100901"}

	r := run("dns.zone.apply", params, Options{})
	if !r.OK || !r.Changed {
		t.Fatalf("new zone: %+v", r)
	}
	file := filepath.Join(p.ZonesDir, "db.example.com")
	if b, _ := os.ReadFile(file); string(b) != zoneV1 {
		t.Errorf("zone file = %q", b)
	}
	if !f.called("named-checkzone example.com ") || !f.called("rndc reconfig") || f.called("rndc reload") {
		t.Errorf("calls = %v", f.calls)
	}
	inc, _ := os.ReadFile(p.Include)
	for _, want := range []string{`zone "example.com" {`, "type master;", `file "` + file + `";`,
		"allow-transfer { 10.0.0.9; 10.0.0.10; };", "also-notify { 10.0.0.9; 10.0.0.10; };"} {
		if !strings.Contains(string(inc), want) {
			t.Errorf("include lacks %q:\n%s", want, inc)
		}
	}
	reg, _ := bind.LoadRegistry(p.Registry())
	if z := reg.Zones["example.com"]; z.Pending || z.Serial != "2026100901" {
		t.Errorf("registry = %+v", z)
	}
	// No staging files left behind.
	entries, _ := os.ReadDir(p.ZonesDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover %s", e.Name())
		}
	}

	// Same again: unchanged, nothing validated or reloaded.
	f.reset()
	r = run("dns.zone.apply", params, Options{})
	if !r.OK || r.Changed || f.called("rndc") || f.called("named-checkzone") {
		t.Fatalf("repeat: %+v %v", r, f.calls)
	}

	// New content: validated, swapped, reloaded (not reconfigured).
	f.reset()
	params["content"] = strings.Replace(zoneV1, "2026100901", "2026100902", 1) + "www IN A 203.0.113.8\n"
	params["serial"] = "2026100902"
	r = run("dns.zone.apply", params, Options{})
	if !r.OK || !r.Changed || !f.called("rndc reload example.com") || f.called("rndc reconfig") {
		t.Fatalf("update: %+v %v", r, f.calls)
	}

	// Different secondaries: stanza change → reconfig.
	f.reset()
	params["secondaries"] = "10.0.0.9"
	r = run("dns.zone.apply", params, Options{})
	if !r.OK || !r.Changed || !f.called("rndc reconfig") || f.called("rndc reload") {
		t.Fatalf("stanza change: %+v %v", r, f.calls)
	}

	// Remove: stanza gone, reconfig, file gone; then unchanged.
	f.reset()
	_ = os.WriteFile(file+".jnl", []byte("j"), 0o644)
	r = run("dns.zone.remove", map[string]string{"zone": "example.com"}, Options{})
	if !r.OK || !r.Changed || !f.called("rndc reconfig") {
		t.Fatalf("remove: %+v %v", r, f.calls)
	}
	if fileExists(file) || fileExists(file+".jnl") {
		t.Error("zone files not removed")
	}
	inc, _ = os.ReadFile(p.Include)
	if strings.Contains(string(inc), "example.com") {
		t.Errorf("stanza still present:\n%s", inc)
	}
	f.reset()
	r = run("dns.zone.remove", map[string]string{"zone": "example.com"}, Options{})
	if !r.OK || r.Changed || f.called("rndc") {
		t.Fatalf("remove again: %+v %v", r, f.calls)
	}
}

func TestZoneApplyRejectedContentLeavesOldFile(t *testing.T) {
	p := readyBind(t)
	f := bindHost(t, p, nil)
	params := map[string]string{"zone": "example.com", "content": zoneV1}
	if r := run("dns.zone.apply", params, Options{}); !r.OK {
		t.Fatal(r.Err)
	}
	f.handler = func(argv []string) (string, error) {
		if argv[0] == "named-checkzone" {
			return "dns_master_load: " + argv[2] + ":3: unknown RR type 'BOGUS'", errors.New("exit status 1")
		}
		return "", nil
	}
	f.reset()
	params["content"] = zoneV1 + "x IN BOGUS 1\n"
	r := run("dns.zone.apply", params, Options{})
	if r.OK || !strings.Contains(r.Err, "rejected") || strings.Contains(r.Err, ".db.example.com.") {
		t.Fatalf("got %+v", r)
	}
	if b, _ := os.ReadFile(filepath.Join(p.ZonesDir, "db.example.com")); string(b) != zoneV1 {
		t.Error("live file was replaced by a rejected zone")
	}
	if f.called("rndc") {
		t.Error("reloaded after a rejected zone")
	}
	entries, _ := os.ReadDir(p.ZonesDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestZoneApplyRetriesFailedReload(t *testing.T) {
	p := readyBind(t)
	fail := true
	f := bindHost(t, p, func(argv []string) (string, error) {
		if argv[0] == "rndc" && fail {
			return "rndc: connect failed: 127.0.0.1#953: connection refused", errors.New("exit status 1")
		}
		return "", errPass
	})
	params := map[string]string{"zone": "example.com", "content": zoneV1}
	r := run("dns.zone.apply", params, Options{})
	if r.OK || !strings.Contains(r.Err, "connection refused") {
		t.Fatalf("got %+v", r)
	}
	reg, _ := bind.LoadRegistry(p.Registry())
	if !reg.Zones["example.com"].Pending {
		t.Fatal("zone not marked pending")
	}
	fail = false
	f.reset()
	r = run("dns.zone.apply", params, Options{})
	if !r.OK || !r.Changed || !f.called("rndc reconfig") {
		t.Fatalf("retry: %+v %v", r, f.calls)
	}
	reg, _ = bind.LoadRegistry(p.Registry())
	if reg.Zones["example.com"].Pending {
		t.Error("still pending after a good reload")
	}
}

func TestZoneApplySecondary(t *testing.T) {
	p := readyBind(t)
	f := bindHost(t, p, nil)
	params := map[string]string{"zone": "example.com", "role": "secondary", "primaries": "203.0.113.7", "serial": "1", "content": "ignored"}
	r := run("dns.zone.apply", params, Options{})
	if !r.OK || !r.Changed || !f.called("rndc reconfig") || f.called("named-checkzone") {
		t.Fatalf("got %+v %v", r, f.calls)
	}
	if fileExists(filepath.Join(p.ZonesDir, "db.example.com")) {
		t.Error("a secondary must not write zone content")
	}
	inc, _ := os.ReadFile(p.Include)
	if !strings.Contains(string(inc), "type slave;") || !strings.Contains(string(inc), "masters { 203.0.113.7; };") {
		t.Errorf("include:\n%s", inc)
	}
	// A new serial with the same stanza: unchanged, but the secondary is nudged.
	f.reset()
	params["serial"] = "2"
	r = run("dns.zone.apply", params, Options{})
	if !r.OK || r.Changed || !f.called("rndc refresh example.com") || f.called("rndc reconfig") {
		t.Fatalf("serial bump: %+v %v", r, f.calls)
	}
	if r := run("dns.zone.apply", map[string]string{"zone": "x.example", "role": "secondary"}, Options{}); r.OK {
		t.Error("secondary without primaries accepted")
	}
}

func TestZoneApplyRefusesBadInput(t *testing.T) {
	p := readyBind(t)
	f := bindHost(t, p, nil)
	cases := []map[string]string{
		{"zone": "../../etc/passwd", "content": zoneV1},
		{"zone": "a/b", "content": zoneV1},
		{"zone": "example.com", "content": "$INCLUDE /etc/shadow\n"},
		{"zone": "example.com", "content": ""},
		{"zone": "example.com", "content": zoneV1, "role": "hidden"},
		{"zone": "example.com", "content": zoneV1, "secondaries": "10.0.0.1; }; include \"/etc/passwd"},
		{"zone": "example.com", "role": "secondary", "primaries": "any"},
		{"zone": "example.com", "content": zoneV1, "serial": "12; drop"},
	}
	for _, c := range cases {
		if r := run("dns.zone.apply", c, Options{}); r.OK {
			t.Errorf("accepted %v", c)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("commands ran for refused input: %v", f.calls)
	}
}

func TestZoneApplyNeedsServerSetUp(t *testing.T) {
	p := tempBind(t) // no main conf at all
	bindHost(t, p, nil)
	r := run("dns.zone.apply", map[string]string{"zone": "example.com", "content": zoneV1}, Options{})
	if r.OK || !strings.Contains(r.Err, "install_dns_server") {
		t.Fatalf("got %+v", r)
	}
}

// ---- disk.mount ------------------------------------------------------------------

type diskFake struct {
	probe   string // blkid export output; "" = blank (exit 2)
	mkfs    int
	mounted bool
}

func diskHost(t *testing.T, d *diskFake) (*fakeHost, string, string) {
	t.Helper()
	dir := t.TempDir()
	oldF, oldM, oldW := fstabPath, mountsPath, deviceWait
	fstabPath = filepath.Join(dir, "fstab")
	mountsPath = filepath.Join(dir, "mounts")
	deviceWait = 50 * time.Millisecond
	t.Cleanup(func() { fstabPath, mountsPath, deviceWait = oldF, oldM, oldW })
	_ = os.WriteFile(fstabPath, []byte("# /etc/fstab\nLABEL=cloudimg-rootfs / ext4 defaults 0 1\n"), 0o644)
	_ = os.WriteFile(mountsPath, []byte("/dev/vda1 / ext4 rw 0 0\n"), 0o644)
	mp := filepath.Join(dir, "mnt", "data")
	f := useFakeHost(t, func(argv []string) (string, error) {
		switch argv[0] {
		case "blkid":
			if d.probe == "" {
				return "", exitErr(t, 2)
			}
			return d.probe, nil
		case "mkfs.ext4", "mkfs.xfs":
			d.mkfs++
			fs := strings.TrimPrefix(argv[0], "mkfs.")
			d.probe = "DEVNAME=/dev/null\nUUID=1111-2222\nTYPE=" + fs + "\n"
			return "", nil
		case "mount":
			d.mounted = true
			_ = os.WriteFile(mountsPath, []byte("/dev/vda1 / ext4 rw 0 0\n/dev/null "+mp+" ext4 rw 0 0\n"), 0o644)
			return "", nil
		}
		return "", nil
	})
	return f, mp, dir
}

func TestDiskMountFormatsBlankOnceAndIsIdempotent(t *testing.T) {
	d := &diskFake{}
	f, mp, _ := diskHost(t, d)
	params := map[string]string{"device": "/dev/null", "mountPoint": mp}

	r := run("disk.mount", params, Options{}) // AllowDestructive is not needed for if_blank
	if !r.OK || !r.Changed || d.mkfs != 1 || !d.mounted {
		t.Fatalf("first: %+v mkfs=%d calls=%v", r, d.mkfs, f.calls)
	}
	if !f.called("mkfs.ext4 -q -F /dev/null") || !f.called("mount "+mp) {
		t.Errorf("calls = %v", f.calls)
	}
	fstab, _ := os.ReadFile(fstabPath)
	line := "UUID=1111-2222 " + mp + " ext4 defaults,nofail,discard 0 2\n"
	if !strings.HasSuffix(string(fstab), line) || !strings.HasPrefix(string(fstab), "# /etc/fstab\n") {
		t.Errorf("fstab = %q", fstab)
	}
	if !fileExists(fstabPath + ".linexus.bak") {
		t.Error("no fstab backup")
	}

	f.reset()
	r = run("disk.mount", params, Options{})
	if !r.OK || r.Changed || d.mkfs != 1 || f.called("mount") {
		t.Fatalf("second: %+v %v", r, f.calls)
	}
	fstab2, _ := os.ReadFile(fstabPath)
	if string(fstab2) != string(fstab) {
		t.Error("fstab line added twice")
	}
}

func TestDiskMountNeverFormatsExistingFilesystem(t *testing.T) {
	d := &diskFake{probe: "DEVNAME=/dev/null\nUUID=abcd\nTYPE=xfs\n"}
	f, mp, _ := diskHost(t, d)
	r := run("disk.mount", map[string]string{"device": "/dev/null", "mountPoint": mp, "fsType": "ext4"}, Options{})
	if !r.OK || d.mkfs != 0 || f.called("mkfs") {
		t.Fatalf("got %+v %v", r, f.calls)
	}
	fstab, _ := os.ReadFile(fstabPath)
	if !strings.Contains(string(fstab), "UUID=abcd "+mp+" xfs defaults,nofail,discard 0 2") {
		t.Errorf("fstab = %q", fstab)
	}
}

func TestDiskMountRefusesPartitionedOrNever(t *testing.T) {
	d := &diskFake{probe: "DEVNAME=/dev/null\nPTUUID=x\nPTTYPE=gpt\n"}
	f, mp, _ := diskHost(t, d)
	r := run("disk.mount", map[string]string{"device": "/dev/null", "mountPoint": mp}, Options{})
	if r.OK || d.mkfs != 0 || !strings.Contains(r.Err, "PTTYPE=gpt") {
		t.Fatalf("partitioned: %+v %v", r, f.calls)
	}
	d.probe = ""
	r = run("disk.mount", map[string]string{"device": "/dev/null", "mountPoint": mp, "format": "never"}, Options{})
	if r.OK || d.mkfs != 0 {
		t.Fatalf("never: %+v", r)
	}
}

func TestDiskMountWaitsThenFails(t *testing.T) {
	d := &diskFake{}
	_, mp, _ := diskHost(t, d)
	start := time.Now()
	r := run("disk.mount", map[string]string{"device": "/dev/linexus-test-missing", "mountPoint": mp}, Options{})
	if r.OK || !strings.Contains(r.Err, "did not appear") || time.Since(start) < 40*time.Millisecond {
		t.Fatalf("got %+v", r)
	}
}

func TestDiskMountRefusesConflicts(t *testing.T) {
	d := &diskFake{probe: "UUID=abcd\nTYPE=ext4\n"}
	_, mp, _ := diskHost(t, d)
	_ = os.WriteFile(mountsPath, []byte("/dev/sdz9 "+mp+" ext4 rw 0 0\n"), 0o644)
	r := run("disk.mount", map[string]string{"device": "/dev/null", "mountPoint": mp}, Options{})
	if r.OK || !strings.Contains(r.Err, "already mounted from /dev/sdz9") {
		t.Fatalf("got %+v", r)
	}
}

func TestDiskMountValidation(t *testing.T) {
	useFakeHost(t, nil)
	for _, c := range []map[string]string{
		{"device": "/dev/null", "mountPoint": "/"},
		{"device": "/dev/null", "mountPoint": "/etc"},
		{"device": "/dev/null", "mountPoint": "/usr"},
		{"device": "/dev/null", "mountPoint": "/proc/x"},
		{"device": "/dev/null", "mountPoint": "relative"},
		{"device": "/dev/null", "mountPoint": "/mnt/a b"},
		{"device": "/dev/null", "mountPoint": "/mnt/../etc"},
		{"device": "/etc/passwd", "mountPoint": "/mnt/data"},
		{"device": "/dev/../etc/passwd", "mountPoint": "/mnt/data"},
		{"device": "/dev/null", "mountPoint": "/mnt/data", "fsType": "btrfs"},
		{"device": "/dev/null", "mountPoint": "/mnt/data", "format": "always"},
		{"mountPoint": "/mnt/data"},
	} {
		if r := run("disk.mount", c, Options{}); r.OK {
			t.Errorf("accepted %v", c)
		}
	}
}

func TestEnsureFstabEntry(t *testing.T) {
	base := "# comment\nUUID=root / ext4 defaults 0 1\n/dev/sdb1 /srv/old xfs defaults 0 0"
	out, added, err := ensureFstabEntry(base, "u1", "/mnt/data", "ext4", nil)
	if err != nil || !added || out != base+"\nUUID=u1 /mnt/data ext4 defaults,nofail,discard 0 2\n" {
		t.Fatalf("add: %q %v %v", out, added, err)
	}
	again, added, err := ensureFstabEntry(out, "u1", "/mnt/data", "ext4", nil)
	if err != nil || added || again != out {
		t.Fatalf("repeat: %v %v", added, err)
	}
	// Present under another name of the same device, or quoted: unchanged.
	for _, existing := range []string{
		"/dev/disk/by-id/scsi-0DO_Volume_v1 /mnt/data ext4 defaults 0 2\n",
		`UUID="u1" /mnt/data ext4 defaults 0 2` + "\n",
		"/dev/disk/by-uuid/u1 /mnt/data ext4 defaults 0 2\n",
	} {
		if _, added, err := ensureFstabEntry(existing, "u1", "/mnt/data", "ext4", []string{"/dev/disk/by-id/scsi-0DO_Volume_v1"}); err != nil || added {
			t.Errorf("%q: added=%v err=%v", existing, added, err)
		}
	}
	if _, _, err := ensureFstabEntry(base, "u2", "/srv/old", "ext4", nil); err == nil {
		t.Error("claimed mount point accepted")
	}
	if _, _, err := ensureFstabEntry("UUID=u1 /elsewhere ext4 defaults 0 2\n", "u1", "/mnt/data", "ext4", nil); err == nil {
		t.Error("volume mounted elsewhere accepted")
	}
	if _, _, err := ensureFstabEntry(`/dev/sdc /mnt/my\040data ext4 defaults 0 2`, "u1", "/mnt/my data", "ext4", nil); err == nil {
		t.Error("escaped mount point not recognised")
	}
	if out, added, _ := ensureFstabEntry("", "u1", "/mnt/x", "xfs", nil); !added || out != "UUID=u1 /mnt/x xfs defaults,nofail,discard 0 2\n" {
		t.Errorf("empty fstab: %q", out)
	}
}

func TestMountedAt(t *testing.T) {
	m := "/dev/vda1 / ext4 rw 0 0\n/dev/sdb /mnt/my\\040data ext4 rw 0 0\n/dev/sdc /mnt/x ext4 rw 0 0\n/dev/sdd /mnt/x ext4 rw 0 0\n"
	if src, ok := mountedAt(m, "/mnt/my data"); !ok || src != "/dev/sdb" {
		t.Errorf("escaped: %q %v", src, ok)
	}
	if src, _ := mountedAt(m, "/mnt/x"); src != "/dev/sdd" {
		t.Errorf("shadowed: %q", src)
	}
	if _, ok := mountedAt(m, "/mnt/none"); ok {
		t.Error("phantom mount")
	}
}

func TestParseBlkidExport(t *testing.T) {
	m := parseBlkidExport("DEVNAME=/dev/sda\nUUID=abc-123\nBLOCK_SIZE=4096\nTYPE=ext4\n")
	if m["UUID"] != "abc-123" || m["TYPE"] != "ext4" || m["DEVNAME"] != "" {
		t.Errorf("got %v", m)
	}
}

func TestUnhandledActionIsSkipped(t *testing.T) {
	if r := run("something.new", nil, Options{}); !r.OK || r.Changed {
		t.Errorf("got %+v", r)
	}
}
