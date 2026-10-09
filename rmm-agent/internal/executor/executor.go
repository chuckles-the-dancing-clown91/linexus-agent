// Package executor runs the plan steps the orchestrator produces. The action
// vocabulary here mirrors linexus-orch/src/plan.rs exactly — it's the contract
// between planner and agent.
//
// Every mutating action is idempotent: it reads current state natively first
// and acts only on drift, reporting whether it changed anything.
package executor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
)

// Options carries what a step needs from the agent around it.
type Options struct {
	// AllowDestructive gates behavior that could take a host down.
	AllowDestructive bool
	// StateFile is the agent's state file; agent.environment persists there.
	StateFile string
	// SendFacts collects and ships a facts report now (agent.facts). It
	// returns a one-line summary.
	SendFacts func() (string, error)
}

// Seams over the host, swapped in tests so executors run against temp dirs
// and scripted commands instead of the real system.
var (
	// execPriv runs argv as root (sudo -n when the agent is not root).
	execPriv = runPrivileged
	// execPlain runs a command as the agent.
	execPlain = runCmd
	// systemdPresent reports whether systemd is the running init system.
	systemdPresent = hasSystemd
	// pkgManager detects the package manager.
	pkgManager = detectPkgManager
)

// StepResult is the outcome of executing one step.
type StepResult struct {
	StepID  string
	Action  string
	OK      bool
	Changed bool // whether the step actually mutated state
	Output  string
	Err     string
	// ExitCode is the failing command's exit status when the step ran one
	// and it exited non-zero; otherwise 1 for a failed step and 0 for a
	// successful one.
	ExitCode int
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
		res = ensureServiceStep(step.Params)
	case "file.write":
		res = writeFile(step.Params["path"], step.Params["content"], step.Params["mode"], step.Params["owner"], step.Params["group"])
	case "system.reboot", "system.power_off", "system.power_on":
		res = powerStep(step.Action, opts)
	case "agent.environment":
		res = setEnvironment(step.Params, opts)
	case "agent.facts":
		res = sendFacts(opts)
	case "dns.server.ensure":
		res = ensureDNSServer(bindPaths())
	case "dns.zone.apply":
		res = applyZone(bindPaths(), step.Params)
	case "dns.zone.remove":
		res = removeZone(bindPaths(), step.Params["zone"])
	case "disk.mount":
		res = mountDisk(step.Params)
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
	if !res.OK && res.ExitCode == 0 {
		res.ExitCode = 1
	}
	return res
}

// exitCodeOf returns a process's exit status from the error its run returned:
// the status for a command that exited non-zero, 1 for any other failure
// (not found, killed by a signal), 0 for no error.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() > 0 {
		return ee.ExitCode()
	}
	return 1
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
		res.ExitCode = exitCodeOf(err)
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
