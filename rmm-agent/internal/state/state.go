// Package state persists what the agent must remember across restarts: its
// identity (agent id and per-agent credential) and the environment an
// operator assigned it. The file is JSON, owner-only (0600), and written
// atomically so a crash mid-write never leaves a half-file that would make the
// agent enroll a duplicate.
//
// Format (every field optional except agentId once enrolled):
//
//	{
//	  "agentId": "0192…",
//	  "agentToken": "nxa_…",
//	  "enrollmentTokenId": "…",
//	  "hostgroup": "acme",
//	  "environment": {"environment": "production", "monitored": true, "note": "", "updatedAt": "…"}
//	}
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Environment is what an operator told this agent it is (the agent.environment
// step). Monitored=false mutes periodic facts reports.
type Environment struct {
	Environment string    `json:"environment"`
	Monitored   bool      `json:"monitored"`
	Note        string    `json:"note,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty"`
}

// Same reports whether two environments carry the same assignment
// (UpdatedAt is bookkeeping, not part of the assignment).
func (e Environment) Same(o Environment) bool {
	return e.Environment == o.Environment && e.Monitored == o.Monitored && e.Note == o.Note
}

type State struct {
	AgentID string `json:"agentId"`
	// AgentToken is the per-agent `nxa_…` credential Nexus issued at
	// enrollment. Empty for legacy agents, which keep using AGENT_TOKEN.
	AgentToken        string `json:"agentToken,omitempty"`
	EnrollmentTokenID string `json:"enrollmentTokenId,omitempty"`
	Hostgroup         string `json:"hostgroup,omitempty"`
	// Environment is nil until an operator (or enrollment) assigned one; a
	// nil environment is treated as tracked production.
	Environment *Environment `json:"environment,omitempty"`
}

// Monitored reports whether periodic facts should be shipped. Anything but an
// explicit "not monitored" leaves the machine tracked.
func (s State) Monitored() bool {
	return s.Environment == nil || s.Environment.Monitored
}

// Load reads the state file. A missing file is not an error — it yields an
// empty State, signalling "not yet enrolled".
func Load(path string) (State, error) {
	var s State
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, nil
}

// Save writes the state file with owner-only permissions, atomically (temp
// file in the same directory, fsync, rename).
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".agent-state-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Update loads the state, applies fn, and saves it — so a writer that changes
// one field never drops another.
func Update(path string, fn func(*State)) (State, error) {
	s, err := Load(path)
	if err != nil {
		return s, err
	}
	fn(&s)
	return s, Save(path, s)
}
