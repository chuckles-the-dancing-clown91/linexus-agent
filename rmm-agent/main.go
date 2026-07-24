// Command rmm-agent is the Linexus infrastructure (RMM) agent. It runs on a
// managed host, enrolls with Nexus, reports facts and liveness, polls for
// tasks, executes their plan steps, and ships results and logs back — all
// through Nexus, the single service it talks to.
//
// This is distinct from the repo's Rust edge-telemetry scaffold (which serves
// the Dignifundus economy's solar/biodigester hardware). This binary manages
// conventional IT infrastructure for Daedalus IT.
package main

import (
	"fmt"
	"log"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/config"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/executor"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/facts"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/state"
)

const agentVersion = "0.1.0"

func main() {
	cfg := config.Load()
	log.Printf("linexus rmm-agent v%s starting (nexus=%s once=%v allowDestructive=%v)",
		agentVersion, cfg.NexusURL, cfg.Once, cfg.AllowDestructive)

	cli := nexus.New(cfg.NexusURL, cfg.Token)
	agentID, err := identify(cli, cfg)
	if err != nil {
		log.Fatalf("identify: %v", err)
	}

	// Report facts on startup (also serves as the first heartbeat).
	f := facts.Collect(agentVersion)
	if err := cli.Report(agentID, f); err != nil {
		log.Printf("report facts failed: %v", err)
	} else {
		log.Printf("reported facts: os=%q kernel=%q arch=%q cpu=%d memMB=%d diskGB=%d uptime=%ds",
			f.OS, f.Kernel, f.Arch, f.CPUCores, f.MemoryMB, f.DiskGB, f.UptimeSeconds)
	}

	if cfg.Once {
		runCycle(cli, agentID, cfg)
		return
	}

	poll := time.NewTicker(cfg.PollInterval)
	heartbeat := time.NewTicker(cfg.HeartbeatInterval)
	defer poll.Stop()
	defer heartbeat.Stop()

	runCycle(cli, agentID, cfg)
	for {
		select {
		case <-heartbeat.C:
			if err := cli.Heartbeat(agentID); err != nil {
				log.Printf("heartbeat: %v", err)
			}
		case <-poll.C:
			runCycle(cli, agentID, cfg)
		}
	}
}

// identify resumes the persisted agent id, or enrolls and persists a new one.
func identify(cli *nexus.Client, cfg config.Config) (string, error) {
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}
	if st.AgentID != "" {
		log.Printf("resuming as agent %s", st.AgentID)
		return st.AgentID, nil
	}

	f := facts.Collect(agentVersion)
	agent, err := cli.Enroll(f.Hostname, cfg.Hostgroup, "")
	if err != nil {
		return "", fmt.Errorf("enroll: %w", err)
	}
	st.AgentID = agent.ID
	if err := state.Save(cfg.StateFile, st); err != nil {
		return "", fmt.Errorf("save state: %w", err)
	}
	log.Printf("enrolled as agent %s (hostname=%s)", agent.ID, agent.Hostname)
	return agent.ID, nil
}

// runCycle sends a heartbeat, polls for tasks, and executes each one.
func runCycle(cli *nexus.Client, agentID string, cfg config.Config) {
	if err := cli.Heartbeat(agentID); err != nil {
		log.Printf("heartbeat: %v", err)
	}
	tasks, err := cli.PollTasks(agentID)
	if err != nil {
		log.Printf("poll tasks: %v", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	log.Printf("received %d task(s)", len(tasks))
	for _, t := range tasks {
		handleTask(cli, agentID, t, cfg)
	}
}

// handleTask executes a task's plan, shipping a log line per step and reporting
// the terminal result. A failed critical step aborts the remaining steps.
func handleTask(cli *nexus.Client, agentID string, t nexus.Task, cfg config.Config) {
	log.Printf("task %s intent=%s steps=%d", t.TaskID, t.Intent, len(t.Plan.Steps))

	logs := make([]nexus.LogEntry, 0, len(t.Plan.Steps))
	ok := true
	firstErr := ""

	for _, step := range t.Plan.Steps {
		r := executor.ExecuteStep(step, executor.Options{AllowDestructive: cfg.AllowDestructive})
		level := "info"
		if !r.OK {
			level = "error"
			ok = false
			if firstErr == "" {
				firstErr = r.Err
			}
		}
		logs = append(logs, nexus.LogEntry{
			Level:    level,
			Source:   "agent",
			Message:  fmt.Sprintf("%s: %s", r.Action, firstNonEmpty(r.Output, r.Err, "ok")),
			TaskID:   t.TaskID,
			Metadata: map[string]any{"stepId": r.StepID, "action": r.Action},
		})
		if !r.OK && step.Critical {
			break
		}
	}

	if err := cli.ShipLogs(agentID, logs); err != nil {
		log.Printf("ship logs: %v", err)
	}

	status := "success"
	if !ok {
		status = "failed"
	}
	msg := fmt.Sprintf("executed %s (%d steps)", t.Intent, len(t.Plan.Steps))
	if err := cli.ReportResult(agentID, t.TaskID, status, firstErr, msg); err != nil {
		log.Printf("report result: %v", err)
	}
	log.Printf("task %s -> %s", t.TaskID, status)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
