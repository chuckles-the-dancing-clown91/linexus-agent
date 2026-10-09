package report

import (
	"strings"
	"testing"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/executor"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
)

func plan(critical ...bool) []nexus.Step {
	actions := []string{"file.write", "command.run", "service.ensure"}
	steps := make([]nexus.Step, len(critical))
	for i, c := range critical {
		steps[i] = nexus.Step{ID: "s" + string(rune('1'+i)), Action: actions[i%len(actions)], Critical: c}
	}
	return steps
}

func TestBuildAllSucceeded(t *testing.T) {
	steps := plan(true, true)
	results := []executor.StepResult{
		{StepID: "s1", Action: "file.write", OK: true, Changed: true, Output: "written"},
		{StepID: "s2", Action: "command.run", OK: true, Changed: true, Output: "deployed"},
	}
	r := Build("deploy", steps, results)

	if r.Status != "success" || r.ExitCode != 0 || r.Error != "" {
		t.Fatalf("got status=%q exit=%d err=%q", r.Status, r.ExitCode, r.Error)
	}
	if r.Message != "executed deploy (2 steps)" {
		t.Errorf("message = %q", r.Message)
	}
	want := "==> file.write [success]\nwritten\n==> command.run [success]\ndeployed"
	if r.Output != want {
		t.Errorf("output = %q, want %q", r.Output, want)
	}
	if len(r.Steps) != 2 || r.Steps[1] != (nexus.StepReport{ID: "s2", Action: "command.run", Status: StepSuccess, Changed: true, Output: "deployed"}) {
		t.Errorf("steps = %+v", r.Steps)
	}
}

func TestBuildCriticalFailureUsesCommandExitCodeAndSkipsRest(t *testing.T) {
	steps := plan(true, true, true)
	results := []executor.StepResult{
		{StepID: "s1", Action: "file.write", OK: true, Changed: true},
		{StepID: "s2", Action: "command.run", OK: false, Changed: true, Output: "boom", Err: "exit status 3", ExitCode: 3},
	}
	r := Build("deploy", steps, results)

	if r.Status != "failed" || r.ExitCode != 3 || r.Error != "exit status 3" {
		t.Fatalf("got status=%q exit=%d err=%q", r.Status, r.ExitCode, r.Error)
	}
	if len(r.Steps) != 3 {
		t.Fatalf("want 3 steps (one skipped), got %d", len(r.Steps))
	}
	if r.Steps[1].Status != StepFailed || r.Steps[1].Error != "exit status 3" || r.Steps[1].Output != "boom" {
		t.Errorf("failed step = %+v", r.Steps[1])
	}
	if r.Steps[2] != (nexus.StepReport{ID: "s3", Action: "service.ensure", Status: StepSkipped}) {
		t.Errorf("skipped step = %+v", r.Steps[2])
	}
	if !strings.Contains(r.Output, "==> command.run [failed]\nboom\nerror: exit status 3") {
		t.Errorf("output = %q", r.Output)
	}
}

func TestBuildCriticalFailureWithoutExitCodeIsOne(t *testing.T) {
	steps := plan(true)
	results := []executor.StepResult{{Action: "file.write", OK: false, Err: "permission denied"}}
	if r := Build("x", steps, results); r.ExitCode != 1 {
		t.Fatalf("exit = %d, want 1", r.ExitCode)
	}
}

func TestBuildNonCriticalFailureKeepsExitZero(t *testing.T) {
	steps := plan(false, true)
	results := []executor.StepResult{
		{Action: "file.write", OK: false, Err: "nope", ExitCode: 1},
		{Action: "command.run", OK: true},
	}
	r := Build("x", steps, results)
	if r.Status != "failed" {
		t.Errorf("status = %q, want failed (any failed step fails the task)", r.Status)
	}
	if r.ExitCode != 0 {
		t.Errorf("exit = %d, want 0 (every critical step succeeded)", r.ExitCode)
	}
}

func TestBuildCapsOutput(t *testing.T) {
	big := strings.Repeat("x", 40*1024) + "END"
	steps := plan(true, true, true)
	results := []executor.StepResult{
		{Action: "a", OK: true, Output: big},
		{Action: "b", OK: true, Output: big},
		{Action: "c", OK: true, Output: big},
	}
	r := Build("x", steps, results)
	if len(r.Output) > OutputCap || !strings.HasPrefix(r.Output, "[... truncated ") || !strings.HasSuffix(r.Output, "END") {
		t.Errorf("combined output not capped to the tail: len=%d", len(r.Output))
	}
	for _, s := range r.Steps {
		if len(s.Output) > StepOutputCap || !strings.HasSuffix(s.Output, "END") {
			t.Errorf("step output not capped: len=%d", len(s.Output))
		}
	}
}

func TestCapKeepsUTF8Boundary(t *testing.T) {
	s := strings.Repeat("é", 100) // 200 bytes
	c := Cap(s, 101)
	if len(c) > 101 || !strings.HasPrefix(c, "[... truncated ") || !strings.HasSuffix(c, "é") {
		t.Errorf("Cap = %q", c)
	}
	if Cap("short", 10) != "short" {
		t.Error("short input must pass through")
	}
}

func TestRefusedFailsAndSkipsEveryStep(t *testing.T) {
	r := Refused("deploy", plan(true, false), "signature verification failed: bad signature")
	if r.Status != "failed" || r.ExitCode != 1 || r.Error != "signature verification failed: bad signature" {
		t.Errorf("result = %+v", r)
	}
	if len(r.Steps) != 2 || r.Steps[0].Status != StepSkipped || r.Steps[1].Status != StepSkipped {
		t.Errorf("steps = %+v", r.Steps)
	}
	if !strings.Contains(r.Output, "bad signature") {
		t.Errorf("output = %q", r.Output)
	}
}
