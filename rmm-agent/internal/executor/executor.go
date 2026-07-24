// Package executor runs the plan steps the orchestrator produces. The action
// vocabulary here mirrors linexus-orch/src/plan.rs exactly — it's the contract
// between planner and agent.
package executor

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
)

// Options gates behavior that could take a host down.
type Options struct {
	AllowDestructive bool
}

// StepResult is the outcome of executing one step.
type StepResult struct {
	StepID string
	Action string
	OK     bool
	Output string
	Err    string
}

// ExecuteStep runs a single plan step and returns its result. It never panics;
// a failure is reported via StepResult, not an error return.
func ExecuteStep(step nexus.Step, opts Options) StepResult {
	res := StepResult{StepID: step.ID, Action: step.Action, OK: true}

	switch step.Action {
	case "command.run":
		command := step.Params["command"]
		if command == "" {
			res.OK = false
			res.Err = "command.run: missing 'command' param"
			return res
		}
		out, err := runShell(command, step.Params["cwd"])
		res.Output = out
		if err != nil {
			res.OK = false
			res.Err = err.Error()
		}

	case "system.reboot", "system.power_off", "system.power_on":
		if !opts.AllowDestructive {
			res.Output = fmt.Sprintf("[dry-run] %s suppressed (set AGENT_ALLOW_DESTRUCTIVE=1 to enable)", step.Action)
			return res
		}
		out, err := runPowerAction(step.Action)
		res.Output = out
		if err != nil {
			res.OK = false
			res.Err = err.Error()
		}

	case "role.provision":
		// Real convergence (packages/services/files for a role) lands in a
		// later phase; for now the step is acknowledged, not executed.
		res.Output = fmt.Sprintf("[stub] role.provision role=%q not yet implemented", step.Params["role"])

	default:
		res.Output = fmt.Sprintf("[skip] unhandled action %q", step.Action)
	}
	return res
}

func runShell(command, cwd string) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", command)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(cmd.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// runPowerAction schedules a destructive action a couple of seconds out so the
// agent can report the result before the box goes down.
func runPowerAction(action string) (string, error) {
	var argv []string
	switch action {
	case "system.reboot":
		argv = []string{"systemctl", "reboot"}
	case "system.power_off":
		argv = []string{"systemctl", "poweroff"}
	case "system.power_on":
		return "", fmt.Errorf("system.power_on requires out-of-band control; not actionable on-host")
	default:
		return "", fmt.Errorf("unknown power action %q", action)
	}
	go func() {
		time.Sleep(2 * time.Second)
		_ = exec.Command(argv[0], argv[1:]...).Run()
	}()
	return fmt.Sprintf("scheduled %s in 2s", action), nil
}
