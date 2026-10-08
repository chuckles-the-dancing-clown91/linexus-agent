package executor

import (
	"testing"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
)

func TestCommandRunReportsExitCode(t *testing.T) {
	r := ExecuteStep(nexus.Step{ID: "s1", Action: "command.run", Params: map[string]string{"command": "echo hi; exit 3"}}, Options{})
	if r.OK || r.ExitCode != 3 || r.Output != "hi" {
		t.Fatalf("got ok=%v exit=%d output=%q", r.OK, r.ExitCode, r.Output)
	}
}

func TestCommandRunSuccessExitZero(t *testing.T) {
	r := ExecuteStep(nexus.Step{Action: "command.run", Params: map[string]string{"command": "true"}}, Options{})
	if !r.OK || r.ExitCode != 0 {
		t.Fatalf("got ok=%v exit=%d", r.OK, r.ExitCode)
	}
}

func TestFailedStepWithoutCommandExitsOne(t *testing.T) {
	r := ExecuteStep(nexus.Step{Action: "command.run", Params: map[string]string{}}, Options{})
	if r.OK || r.ExitCode != 1 {
		t.Fatalf("got ok=%v exit=%d", r.OK, r.ExitCode)
	}
}
