// Package executor runs the plan steps the orchestrator produces. The action
// vocabulary here mirrors linexus-orch/src/plan.rs exactly — it's the contract
// between planner and agent.
//
// Every mutating action is idempotent: it reads current state natively first
// and acts only on drift, reporting whether it changed anything.
package executor

import (
	"fmt"
	"os"
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
	StepID  string
	Action  string
	OK      bool
	Changed bool // whether the step actually mutated state
	Output  string
	Err     string
}

// ExecuteStep runs a single plan step and returns its result. It never panics;
// a failure is reported via StepResult, not an error return.
func ExecuteStep(step nexus.Step, opts Options) StepResult {
	var res StepResult
	switch step.Action {
	case "command.run":
		res = runCommandStep(step)
	case "package.ensure":
		res = ensurePackage(step.Params["name"], step.Params["state"], step.Params["version"])
	case "service.ensure":
		res = ensureService(step.Params["name"], step.Params["state"], step.Params["enabled"])
	case "file.write":
		res = writeFile(step.Params["path"], step.Params["content"], step.Params["mode"], step.Params["owner"], step.Params["group"])
	case "system.reboot", "system.power_off", "system.power_on":
		res = powerStep(step.Action, opts)
	case "role.provision":
		res = StepResult{
			OK:     true,
			Output: fmt.Sprintf("[stub] role.provision role=%q — expand this role in the orchestrator", step.Params["role"]),
		}
	default:
		res = StepResult{OK: true, Output: fmt.Sprintf("[skip] unhandled action %q", step.Action)}
	}
	res.StepID = step.ID
	res.Action = step.Action
	return res
}

func runCommandStep(step nexus.Step) StepResult {
	res := StepResult{OK: true}
	command := step.Params["command"]
	if command == "" {
		res.OK = false
		res.Err = "command.run: missing 'command' param"
		return res
	}
	out, err := runShell(command, step.Params["cwd"])
	res.Output = out
	res.Changed = true // a command is assumed to have an effect
	if err != nil {
		res.OK = false
		res.Err = err.Error()
	}
	return res
}

func powerStep(action string, opts Options) StepResult {
	res := StepResult{OK: true}
	if !opts.AllowDestructive {
		res.Output = fmt.Sprintf("[dry-run] %s suppressed (set AGENT_ALLOW_DESTRUCTIVE=1 to enable)", action)
		return res
	}
	out, err := runPowerAction(action)
	res.Output = out
	res.Changed = true
	if err != nil {
		res.OK = false
		res.Err = err.Error()
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

// runCmd runs a command and returns trimmed combined output.
func runCmd(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\n"), err
}

// runPrivileged runs argv as root, prefixing `sudo -n` when the agent isn't
// already root so it never blocks on a password prompt.
func runPrivileged(argv ...string) (string, error) {
	if os.Geteuid() == 0 {
		return runCmd(argv[0], argv[1:]...)
	}
	return runCmd("sudo", append([]string{"-n"}, argv...)...)
}
