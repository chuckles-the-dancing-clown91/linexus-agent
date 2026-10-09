package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingIsEmpty(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || s.AgentID != "" || !s.Monitored() {
		t.Fatalf("got %+v err=%v", s, err)
	}
}

func TestSaveRoundTripIsOwnerOnly(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	in := State{AgentID: "a1", AgentToken: "nxa_secret", Hostgroup: "acme",
		Environment: &Environment{Environment: "staging", Monitored: false, Note: "lab"}}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	out, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if out.AgentID != "a1" || out.AgentToken != "nxa_secret" || out.Hostgroup != "acme" ||
		out.Environment == nil || !out.Environment.Same(*in.Environment) {
		t.Errorf("round trip = %+v", out)
	}
	if out.Monitored() {
		t.Error("monitored=false did not survive")
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Errorf("dir has %d entries, want 1", len(entries))
	}
}

func TestLegacyStateFileStillLoads(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(p, []byte(`{"agentId": "legacy-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(p)
	if err != nil || s.AgentID != "legacy-1" || s.AgentToken != "" || !s.Monitored() {
		t.Fatalf("got %+v err=%v", s, err)
	}
}

func TestUpdateKeepsOtherFields(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	if err := Save(p, State{AgentID: "a1", AgentToken: "nxa_x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(p, func(s *State) {
		s.Environment = &Environment{Environment: "production", Monitored: true}
	}); err != nil {
		t.Fatal(err)
	}
	s, _ := Load(p)
	if s.AgentID != "a1" || s.AgentToken != "nxa_x" || s.Environment == nil {
		t.Errorf("got %+v", s)
	}
}

func TestEnvironmentSameIgnoresTimestamp(t *testing.T) {
	a := Environment{Environment: "dev", Monitored: true}
	b := a
	b.UpdatedAt = b.UpdatedAt.AddDate(1, 0, 0)
	if !a.Same(b) {
		t.Error("UpdatedAt should not matter")
	}
	b.Note = "x"
	if a.Same(b) {
		t.Error("note should matter")
	}
}
