// Package report folds the outcomes of a task's plan steps into the result
// body the agent posts to Nexus: the overall verdict, an exit code, combined
// output, and one entry per planned step.
package report

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/executor"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
)

// Size caps, in bytes. Nexus applies the same limits on its side; capping here
// keeps a chatty script from turning one result post into megabytes.
const (
	OutputCap     = 64 * 1024 // combined output
	StepOutputCap = 16 * 1024 // one step's output
	StepErrorCap  = 4 * 1024  // one step's error
)

// Step statuses as reported to Nexus.
const (
	StepSuccess = "success"
	StepFailed  = "failed"
	StepSkipped = "skipped"
)

// Build turns the results of running a plan into the result body.
//
// results holds one entry per step that ran, in plan order; steps after the
// last result were not run (an earlier critical step failed) and are reported
// as skipped. The verdict is "success" only when every step that ran
// succeeded. ExitCode is 0 when every critical step succeeded, otherwise the
// exit code of the first critical step that failed (its command's exit status,
// or 1). Output is the steps' output in order, each under a "==> action
// [status]" header, keeping the tail when it exceeds OutputCap.
func Build(intent string, steps []nexus.Step, results []executor.StepResult) nexus.Result {
	out := nexus.Result{
		Status:  "success",
		Message: fmt.Sprintf("executed %s (%d steps)", intent, len(steps)),
		Steps:   make([]nexus.StepReport, 0, len(steps)),
	}
	exitSet := false
	var blocks []string

	for i, r := range results {
		status := StepSuccess
		if !r.OK {
			status = StepFailed
			if out.Status == "success" {
				out.Status = "failed"
				out.Error = r.Err
			}
			critical := i < len(steps) && steps[i].Critical
			if critical && !exitSet {
				out.ExitCode = r.ExitCode
				if out.ExitCode == 0 {
					out.ExitCode = 1
				}
				exitSet = true
			}
		}
		out.Steps = append(out.Steps, nexus.StepReport{
			ID:      r.StepID,
			Action:  r.Action,
			Status:  status,
			Changed: r.Changed,
			Output:  Cap(r.Output, StepOutputCap),
			Error:   Cap(r.Err, StepErrorCap),
		})
		if r.Output != "" || r.Err != "" {
			block := fmt.Sprintf("==> %s [%s]", r.Action, status)
			if r.Output != "" {
				block += "\n" + r.Output
			}
			if r.Err != "" {
				block += "\nerror: " + r.Err
			}
			blocks = append(blocks, block)
		}
	}

	for i := len(results); i < len(steps); i++ {
		out.Steps = append(out.Steps, nexus.StepReport{
			ID:     steps[i].ID,
			Action: steps[i].Action,
			Status: StepSkipped,
		})
	}

	out.Output = Cap(strings.Join(blocks, "\n"), OutputCap)
	return out
}

// Cap keeps at most max bytes of s. Longer text keeps its tail — where a
// failing script says why — behind a marker saying how much was dropped. The
// cut lands on a UTF-8 boundary, so the result may be a few bytes under max.
func Cap(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const markerRoom = 48
	keep := max - markerRoom
	if keep < 0 {
		keep = 0
	}
	start := len(s) - keep
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return fmt.Sprintf("[... truncated %d bytes ...]\n%s", start, s[start:])
}
