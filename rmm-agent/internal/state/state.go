// Package state persists the agent's assigned identity so a restart resumes as
// the same machine rather than enrolling a duplicate.
package state

import (
	"encoding/json"
	"os"
)

type State struct {
	AgentID string `json:"agentId"`
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

// Save writes the state file with owner-only permissions.
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}
