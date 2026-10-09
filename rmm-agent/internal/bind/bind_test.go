package bind

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeZone(t *testing.T) {
	good := map[string]string{
		"Example.COM":            "example.com",
		"example.com.":           "example.com",
		"a-b.example.co.uk":      "a-b.example.co.uk",
		"1.168.192.in-addr.arpa": "1.168.192.in-addr.arpa",
		"xn--bcher-kva.example":  "xn--bcher-kva.example",
		"localhost":              "localhost",
	}
	for in, want := range good {
		got, err := NormalizeZone(in)
		if err != nil || got != want {
			t.Errorf("NormalizeZone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", ".", "..", "a..b", "../etc", "a/b", "example.com/../../x", "-a.com", "a-.com",
		"ex ample.com", `ex"ample.com`, "ex;ample.com", "under_score.com", "a.com..",
		strings.Repeat("a", 64) + ".com", strings.Repeat("abcdefghi.", 26) + "com",
		"zone\nname", "é.com",
	}
	for _, in := range bad {
		if got, err := NormalizeZone(in); err == nil {
			t.Errorf("NormalizeZone(%q) = %q, want error", in, got)
		}
	}
}

func TestParseAddrList(t *testing.T) {
	got, err := ParseAddrList(" 10.0.0.1, 10.0.0.2;2001:db8::1 10.0.0.1 ")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "10.0.0.1,10.0.0.2,2001:db8::1" {
		t.Errorf("got %v", got)
	}
	if got, err := ParseAddrList(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"10.0.0.1, any", "10.0.0.0/8", "10.0.0.1; }; include \"/etc/passwd\""} {
		if _, err := ParseAddrList(bad); err == nil {
			t.Errorf("ParseAddrList(%q) accepted", bad)
		}
	}
}

func TestCheckZoneContent(t *testing.T) {
	if err := CheckZoneContent("$TTL 3600\n@ IN SOA ns1 hostmaster 1 2 3 4 5\n"); err != nil {
		t.Errorf("valid content refused: %v", err)
	}
	for _, bad := range []string{"", "  \n", "$INCLUDE /etc/shadow\n", "$ttl 1\n  $include /etc/passwd\n", "a\x00b"} {
		if err := CheckZoneContent(bad); err == nil {
			t.Errorf("CheckZoneContent(%q) accepted", bad)
		}
	}
}

func TestRenderIncludeIsDeterministicAndCorrect(t *testing.T) {
	r := Registry{Zones: map[string]Zone{
		"zeta.example":  {Role: "secondary", File: "/z/db.zeta.example", Primaries: []string{"10.0.0.1", "10.0.0.2"}},
		"alpha.example": {Role: "primary", File: "/z/db.alpha.example", Secondaries: []string{"10.0.0.9"}, Serial: "2026100901"},
		"mid.example":   {Role: "primary", File: "/z/db.mid.example"},
	}}
	got := RenderInclude(r)
	if got != RenderInclude(r) {
		t.Fatal("render is not deterministic")
	}
	want := `zone "alpha.example" {
    type master;
    file "/z/db.alpha.example";
    allow-transfer { 10.0.0.9; };
    also-notify { 10.0.0.9; };
};

zone "mid.example" {
    type master;
    file "/z/db.mid.example";
    allow-transfer { none; };
};

zone "zeta.example" {
    type slave;
    masters { 10.0.0.1; 10.0.0.2; };
    file "/z/db.zeta.example";
};
`
	if !strings.HasSuffix(got, want) {
		t.Errorf("render =\n%s\nwant suffix\n%s", got, want)
	}
	if strings.Index(got, "alpha") > strings.Index(got, "zeta") {
		t.Error("zones not sorted")
	}
	empty := RenderInclude(Registry{Zones: map[string]Zone{}})
	if strings.Contains(empty, "zone ") {
		t.Errorf("empty registry rendered zones: %q", empty)
	}
}

func TestSameStanzaIgnoresSerialAndPending(t *testing.T) {
	a := Zone{Role: "primary", File: "/f", Secondaries: []string{"1.1.1.1"}, Serial: "1"}
	b := a
	b.Serial, b.Pending = "2", true
	if !a.SameStanza(b) {
		t.Error("serial/pending should not change the stanza")
	}
	b.Secondaries = []string{"1.1.1.1", "2.2.2.2"}
	if a.SameStanza(b) {
		t.Error("secondaries should change the stanza")
	}
}

func TestRegistryRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "linexus-zones.json")
	r, err := LoadRegistry(p)
	if err != nil || len(r.Zones) != 0 {
		t.Fatalf("missing registry: %+v %v", r, err)
	}
	r.Zones["b.example"] = Zone{Role: "primary", File: "/x"}
	r.Zones["a.example"] = Zone{Role: "secondary", File: "/y", Primaries: []string{"10.0.0.1"}}
	if err := os.WriteFile(p, r.Marshal(), 0o644); err != nil {
		t.Fatal(err)
	}
	r2, err := LoadRegistry(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r2.Names(), ",") != "a.example,b.example" {
		t.Errorf("names = %v", r2.Names())
	}
	if string(r.Marshal()) != string(r2.Marshal()) {
		t.Error("marshal is not stable")
	}
}

func TestIncludeLine(t *testing.T) {
	inc := "/etc/bind/linexus-zones.conf"
	conf := "//\n// Do any local configuration here\n//\n"
	out, changed := WithIncludeLine(conf, inc)
	if !changed || !HasIncludeLine(out, inc) {
		t.Fatalf("not added: %q", out)
	}
	again, changed := WithIncludeLine(out, inc)
	if changed || again != out {
		t.Error("include line added twice")
	}
	commented := "// include \"/etc/bind/linexus-zones.conf\";\n"
	if HasIncludeLine(commented, inc) {
		t.Error("a commented include must not count")
	}
	noNL := `options { };`
	out, _ = WithIncludeLine(noNL, inc)
	if !strings.HasPrefix(out, "options { };\n") {
		t.Errorf("missing newline handling: %q", out)
	}
}

func TestPathsFromEnvOverrides(t *testing.T) {
	env := map[string]string{
		"AGENT_BIND_CONF_DIR":  "/tmp/conf",
		"AGENT_BIND_ZONES_DIR": "/tmp/zones",
		"AGENT_BIND_SERVICE":   "fake-named",
		"AGENT_BIND_CHECKZONE": "/bin/true",
		"AGENT_BIND_RNDC":      "/bin/echo",
	}
	p := PathsFromEnv(func(k string) string { return env[k] })
	if p.Include != "/tmp/conf/linexus-zones.conf" || p.ZonesDir != "/tmp/zones" ||
		p.Service != "fake-named" || p.CheckZone != "/bin/true" || p.Rndc != "/bin/echo" ||
		!strings.HasPrefix(p.MainConf, "/tmp/conf/") {
		t.Errorf("paths = %+v", p)
	}
	if p.Registry() != "/tmp/zones/linexus-zones.json" || p.ZoneFile("a.example") != "/tmp/zones/db.a.example" {
		t.Errorf("derived paths wrong: %s %s", p.Registry(), p.ZoneFile("a.example"))
	}
	env["AGENT_BIND_INCLUDE"] = "/elsewhere/z.conf"
	if PathsFromEnv(func(k string) string { return env[k] }).Include != "/elsewhere/z.conf" {
		t.Error("AGENT_BIND_INCLUDE ignored")
	}
}
