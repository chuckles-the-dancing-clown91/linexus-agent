package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/state"
)

// setEnvironment is agent.environment: params environment (default
// production), monitored (anything but an explicit false/0/no is true), note.
// The assignment is persisted in the state file; while monitored is false
// the agent stops shipping periodic facts reports (it still heartbeats and
// takes tasks, so it can be un-muted). Identical assignments are unchanged.
func setEnvironment(p map[string]string, opts Options) StepResult {
	res := StepResult{OK: true}
	if opts.StateFile == "" {
		res.OK = false
		res.Err = "agent.environment: the agent has no state file to persist to"
		return res
	}
	want := state.Environment{
		Environment: strings.TrimSpace(p["environment"]),
		Monitored:   true,
		Note:        strings.TrimSpace(p["note"]),
	}
	if want.Environment == "" {
		want.Environment = "production"
	}
	switch strings.ToLower(strings.TrimSpace(p["monitored"])) {
	case "false", "0", "no", "off":
		want.Monitored = false
	}
	if len(want.Environment) > 64 || len(want.Note) > 1000 {
		res.OK = false
		res.Err = "agent.environment: environment (64) or note (1000) too long"
		return res
	}

	cur, err := state.Load(opts.StateFile)
	if err != nil {
		res.OK = false
		res.Err = fmt.Sprintf("agent.environment: load state: %v", err)
		return res
	}
	describe := func(e state.Environment) string {
		s := e.Environment
		if e.Monitored {
			s += " (monitored)"
		} else {
			s += " (not monitored: periodic facts reports paused)"
		}
		return s
	}
	if cur.Environment != nil && cur.Environment.Same(want) {
		res.Output = "environment already " + describe(want)
		return res
	}
	want.UpdatedAt = time.Now().UTC()
	if _, err := state.Update(opts.StateFile, func(s *state.State) { s.Environment = &want }); err != nil {
		res.OK = false
		res.Err = fmt.Sprintf("agent.environment: save state: %v", err)
		return res
	}
	res.Changed = true
	res.Output = "environment set to " + describe(want)
	return res
}

// sendFacts is agent.facts: collect and ship a facts report now. It is an
// explicit operator request, so it is sent even while the agent is muted.
func sendFacts(opts Options) StepResult {
	if opts.SendFacts == nil {
		return StepResult{Err: "agent.facts: facts reporting is not available in this context"}
	}
	out, err := opts.SendFacts()
	if err != nil {
		return StepResult{Output: out, Err: fmt.Sprintf("agent.facts: %v", err)}
	}
	return StepResult{OK: true, Output: out}
}
