package facts

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/bind"
)

const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:1538 00000000:0000 0A 00000000:00000000 00:00000000 00000000   113        0 1002 1 0000000000000000 100 0 0 10 0
   2: 0500000A:0016 0200000A:C350 01 00000000:00000000 02:000A7B2C 00000000     0        0 1003 4 0000000000000000 20 4 30 10 -1
   3: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1004 1 0000000000000000 100 0 0 10 0
`

const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0050 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2001 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:0277 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2002 1 0000000000000000 100 0 0 10 0
   2: B80D0120000000000000000001000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2003 1 0000000000000000 100 0 0 10 0
   3: 0000000000000000FFFF00000100007F:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2004 1 0000000000000000 100 0 0 10 0
`

const procNetUDP = `   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops
  100: 3500007F:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000   102        0 3001 2 0000000000000000 0
  101: 0500000A:A1B2 0800000A:0035 01 00000000:00000000 00:00000000 00000000     0        0 3002 2 0000000000000000 0
  102: 00000000:0044 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 3003 2 0000000000000000 0
`

func TestParseProcNetTCP(t *testing.T) {
	got := ParseProcNet(strings.NewReader(procNetTCP), "tcp", binary.LittleEndian)
	want := []Listener{
		{Proto: "tcp", Address: "0.0.0.0", Port: 22, inode: "1001"},
		{Proto: "tcp", Address: "127.0.0.1", Port: 5432, inode: "1002"},
		{Proto: "tcp", Address: "0.0.0.0", Port: 22, inode: "1004"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseProcNetTCP6(t *testing.T) {
	got := ParseProcNet(strings.NewReader(procNetTCP6), "tcp", binary.LittleEndian)
	want := []string{"[::]:80", "[::1]:631", "[2001:db8::1]:443", "[::ffff:127.0.0.1]:8080"}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if g := net.JoinHostPort(got[i].Address, itoa(got[i].Port)); g != w {
			t.Errorf("[%d] = %s, want %s", i, g, w)
		}
	}
}

func TestParseProcNetUDPKeepsOnlyUnconnected(t *testing.T) {
	got := ParseProcNet(strings.NewReader(procNetUDP), "udp", binary.LittleEndian)
	if len(got) != 2 || got[0].Address != "127.0.0.53" || got[0].Port != 53 || got[1].Port != 68 {
		t.Fatalf("got %+v", got)
	}
}

func TestParseProcNetBigEndianHost(t *testing.T) {
	in := "hdr\n 0: 7F000001:0016 00000000:0000 0A 0:0 0:0 0 0 0 42 1\n"
	got := ParseProcNet(strings.NewReader(in), "tcp", binary.BigEndian)
	if len(got) != 1 || got[0].Address != "127.0.0.1" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseProcNetToleratesGarbage(t *testing.T) {
	in := "header\nshort line\n 0: ZZZ:0016 00000000:0000 0A a b c d e 9 1\n 1: 0100007F:XYZ 0:0 0A a b c d e 9 1\n"
	if got := ParseProcNet(strings.NewReader(in), "tcp", binary.LittleEndian); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func jsonInt(i int) string { return strconv.Itoa(i) }

func TestSocketOwnersAndListeningDedupe(t *testing.T) {
	root := t.TempDir()
	mk := func(pid, comm string, inodes ...string) {
		fd := filepath.Join(root, pid, "fd")
		if err := os.MkdirAll(fd, 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(root, pid, "comm"), []byte(comm+"\n"), 0o644)
		for i, in := range inodes {
			if err := os.Symlink("socket:["+in+"]", filepath.Join(fd, jsonInt(i+3))); err != nil {
				t.Fatal(err)
			}
		}
		_ = os.Symlink("/dev/null", filepath.Join(fd, "0"))
	}
	mk("100", "sshd", "1001", "1004")
	mk("200", "postgres", "1002")
	_ = os.MkdirAll(filepath.Join(root, "self"), 0o755)

	netDir := filepath.Join(root, "net")
	_ = os.MkdirAll(netDir, 0o755)
	_ = os.WriteFile(filepath.Join(netDir, "tcp"), []byte(procNetTCP), 0o644)
	_ = os.WriteFile(filepath.Join(netDir, "udp"), []byte(procNetUDP), 0o644)

	old := procRoot
	procRoot = root
	defer func() { procRoot = old }()

	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("fixtures are little-endian")
	}
	got := Listening()
	// 0.0.0.0:22 appears twice in the table; once in the report.
	var lines []string
	for _, l := range got {
		lines = append(lines, l.Proto+" "+l.Address+":"+jsonInt(l.Port)+" "+l.Process)
	}
	want := "tcp 0.0.0.0:22 sshd|tcp 127.0.0.1:5432 postgres|udp 127.0.0.53:53 |udp 0.0.0.0:68 "
	if strings.Join(lines, "|") != want {
		t.Errorf("got  %q\nwant %q", strings.Join(lines, "|"), want)
	}
}

func TestPublicIPv4(t *testing.T) {
	ifs := []Interface{
		{Name: "lo", Up: true, Loopback: true, Addresses: []string{"127.0.0.1/8", "8.8.8.8/32"}},
		{Name: "docker0", Up: false, Addresses: []string{"1.2.3.4/16"}},
		{Name: "eth0", Up: true, Addresses: []string{"fe80::1/64", "10.10.0.5/16", "100.64.1.1/10", "169.254.1.1/16", "203.0.113.7/20"}},
		{Name: "eth1", Up: true, Addresses: []string{"198.51.100.9/24"}},
	}
	if got := PublicIPv4(ifs); got != "203.0.113.7" {
		t.Errorf("got %q", got)
	}
	private := []Interface{{Name: "eth0", Up: true, Addresses: []string{"192.168.1.10/24", "172.20.0.1/16", "100.127.255.1/10"}}}
	if got := PublicIPv4(private); got != "" {
		t.Errorf("private host reported %q", got)
	}
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "100.63.255.255": true, "100.64.0.0": false, "100.128.0.1": true,
		"172.15.0.1": true, "172.16.0.1": false, "224.0.0.1": false, "0.0.0.0": false, "2001:db8::1": false,
	} {
		if got := IsPublicIPv4(net.ParseIP(ip)); got != want {
			t.Errorf("IsPublicIPv4(%s) = %v", ip, got)
		}
	}
}

func TestParseServices(t *testing.T) {
	units := `nginx.service                loaded    active   running A high performance web server
● postgresql@16-main.service  loaded    failed   failed  PostgreSQL Cluster 16-main
ssh.service                  loaded    active   running OpenBSD Secure Shell server
bogus line
apt-daily.timer              loaded    active   waiting Daily apt download activities
systemd-journald.service     loaded    active   running Journal Service
`
	files := `nginx.service                 enabled         enabled
postgresql@.service           enabled-runtime enabled
ssh.service                   disabled        enabled
systemd-journald.service      static          -
`
	got := ParseServices(units, files)
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	byName := map[string]Service{}
	for _, s := range got {
		byName[s.Name] = s
	}
	n := byName["nginx.service"]
	if n.State != "active" || n.Detail != "running" || !n.Enabled || n.Description != "A high performance web server" {
		t.Errorf("nginx = %+v", n)
	}
	if p := byName["postgresql@16-main.service"]; p.State != "failed" || !p.Enabled {
		t.Errorf("postgres instance = %+v", p)
	}
	if byName["ssh.service"].Enabled || byName["systemd-journald.service"].Enabled {
		t.Error("disabled/static reported enabled")
	}
	if got[0].Name != "nginx.service" {
		t.Errorf("not sorted: %v", got[0].Name)
	}
}

func TestServicesWithoutSystemdIsNil(t *testing.T) {
	old := systemdRunning
	systemdRunning = func() bool { return false }
	defer func() { systemdRunning = old }()
	if s := Services(); s != nil {
		t.Errorf("got %+v", s)
	}
}

func TestParseDpkgKeepsInstalledOnly(t *testing.T) {
	out := "nginx\t1.24.0-2ubuntu7\tinstall ok installed\n" +
		"oldpkg\t1.0\tdeinstall ok config-files\n" +
		"bash\t5.2.21-2ubuntu4\tinstall ok installed\n" +
		"\n" +
		"nostatus\t2.0\n"
	got := ParseDpkg(out)
	if len(got) != 3 || got[0].Name != "bash" || got[1].Name != "nginx" || got[2].Name != "nostatus" || got[1].Manager != "apt" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseRPMAndCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("gpg-pubkey\tabc-def\n")
	for i := 0; i < MaxPackages+50; i++ {
		b.WriteString("pkg" + jsonInt(100000+i) + "\t1.0-1.el9\n")
	}
	got := ParseRPM(b.String())
	if len(got) != MaxPackages {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Name != "pkg100000" || got[0].Version != "1.0-1.el9" || got[0].Manager != "dnf" {
		t.Errorf("first = %+v", got[0])
	}
}

func TestPackagesToleratesMissingTools(t *testing.T) {
	oldLP := lookPath
	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	defer func() { lookPath = oldLP }()
	if p := Packages(); p != nil {
		t.Errorf("got %+v", p)
	}
	if d := DetectDNSServer(); d != nil && !fileExists("/usr/sbin/named") {
		t.Errorf("got %+v", d)
	}
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestParseNamedVersion(t *testing.T) {
	for in, want := range map[string]string{
		"BIND 9.18.28-0ubuntu0.22.04.1-Ubuntu (Extended Support Version) <id:>\n": "9.18.28",
		"BIND 9.16.23-RH (Extended Support Version) <id:fde3b1f>":                 "9.16.23",
		"BIND 9.20": "9.20",
	} {
		if got := ParseNamedVersion(in); got != want {
			t.Errorf("ParseNamedVersion(%q) = %q", in, got)
		}
	}
}

func TestDetectDNSServerWithFakes(t *testing.T) {
	zones := t.TempDir()
	reg := bind.Registry{Zones: map[string]bind.Zone{
		"b.example": {Role: "primary"}, "a.example": {Role: "secondary"},
	}}
	if err := os.WriteFile(filepath.Join(zones, "linexus-zones.json"), reg.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	oldLP, oldRun, oldSD, oldBP := lookPath, runTool, systemdRunning, bindPaths
	defer func() { lookPath, runTool, systemdRunning, bindPaths = oldLP, oldRun, oldSD, oldBP }()
	lookPath = func(name string) (string, error) {
		if name == "named" {
			return "/usr/sbin/named", nil
		}
		return "", errors.New("no")
	}
	var calls []string
	runTool = func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "/usr/sbin/named":
			return "BIND 9.18.28-1~deb12u2-Debian (Extended Support Version) <id:>\n", nil
		case name == "systemctl" && args[len(args)-1] == "named":
			return "", errors.New("inactive")
		case name == "systemctl" && args[len(args)-1] == "bind9":
			return "", nil
		}
		return "", errors.New("unexpected")
	}
	systemdRunning = func() bool { return true }
	bindPaths = func() bind.Paths { return bind.Paths{ZonesDir: zones} }

	d := DetectDNSServer()
	if d == nil || d.Software != "bind9" || d.Version != "9.18.28" || !d.Running ||
		strings.Join(d.Zones, ",") != "a.example,b.example" {
		t.Fatalf("got %+v (calls %v)", d, calls)
	}
}

func TestFactsJSONFieldNames(t *testing.T) {
	f := Facts{
		Hostname: "h", MachineID: "m", PublicIP: "203.0.113.7",
		Interfaces: []Interface{{Name: "eth0", MAC: "aa", Up: true, Loopback: false, Addresses: []string{"203.0.113.7/20"}}},
		Listening:  []Listener{{Proto: "tcp", Address: "0.0.0.0", Port: 443, Process: "nginx"}},
		Services:   []Service{{Name: "nginx.service", State: "active", Detail: "running", Enabled: true, Description: "d"}},
		Packages:   []Package{{Name: "nginx", Version: "1", Manager: "apt"}},
		DNSServer:  &DNSServer{Software: "bind9", Version: "9.18", Running: true, Zones: []string{"example.com"}},
	}
	b, _ := json.Marshal(f)
	s := string(b)
	for _, want := range []string{
		`"machineId":"m"`, `"publicIp":"203.0.113.7"`,
		`"interfaces":[{"name":"eth0","mac":"aa","up":true,"addresses":["203.0.113.7/20"]}]`,
		`"listening":[{"proto":"tcp","address":"0.0.0.0","port":443,"process":"nginx"}]`,
		`"services":[{"name":"nginx.service","state":"active","detail":"running","enabled":true,"description":"d"}]`,
		`"packages":[{"name":"nginx","version":"1","manager":"apt"}]`,
		`"dnsServer":{"software":"bind9","version":"9.18","running":true,"zones":["example.com"]}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s in %s", want, s)
		}
	}
	empty, _ := json.Marshal(Facts{Hostname: "h"})
	for _, k := range []string{"machineId", "publicIp", "interfaces", "listening", "services", "packages", "dnsServer"} {
		if strings.Contains(string(empty), k) {
			t.Errorf("empty %s should be omitted: %s", k, empty)
		}
	}
}

func TestCollectFullNeverPanics(t *testing.T) {
	f := CollectFull("test")
	if f.AgentVersion != "test" || f.Hostname == "" {
		t.Errorf("got %+v", f)
	}
}
