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
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/config"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/executor"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/facts"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/report"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/state"
)

const agentVersion = "0.2.0"

// agent is the running agent: its Nexus client, identity and configuration.
type agent struct {
	cli  *nexus.Client
	cfg  config.Config
	id   string
	auth *authWatch

	mu    sync.Mutex
	muted bool // last observed "not monitored" state, for logging transitions
}

func main() {
	cfg := config.Load()
	log.Printf("linexus rmm-agent v%s starting (nexus=%s once=%v allowDestructive=%v facts=%s)",
		agentVersion, cfg.NexusURL, cfg.Once, cfg.AllowDestructive, cfg.FactsInterval)

	cli := nexus.New(cfg.NexusURL, cfg.Token)
	st, err := identify(cli, cfg)
	if err != nil {
		log.Fatalf("identify: %v", err)
	}
	a := &agent{cli: cli, cfg: cfg, id: st.AgentID,
		auth: &authWatch{stateFile: cfg.StateFile, perAgent: st.AgentToken != ""}}

	// Facts on startup (also the first heartbeat). Always sent once, even
	// for an agent that is not monitored, so Nexus knows what it is.
	if summary, err := a.sendFacts(); err != nil {
		a.auth.note("report facts", err)
	} else {
		log.Print(summary)
	}

	if cfg.Once {
		a.runCycle()
		return
	}

	poll := time.NewTicker(cfg.PollInterval)
	heartbeat := time.NewTicker(cfg.HeartbeatInterval)
	factsTick := time.NewTicker(cfg.FactsInterval)
	defer poll.Stop()
	defer heartbeat.Stop()
	defer factsTick.Stop()

	a.runCycle()
	for {
		select {
		case <-heartbeat.C:
			if err := a.cli.Heartbeat(a.id); err != nil {
				a.auth.note("heartbeat", err)
			}
		case <-factsTick.C:
			a.periodicFacts()
		case <-poll.C:
			a.runCycle()
		}
	}
}

// identify resumes the persisted identity, or enrolls and persists a new one,
// and points the client at the right credential: the per-agent token from
// the state file, else (a legacy agent) AGENT_TOKEN.
func identify(cli *nexus.Client, cfg config.Config) (state.State, error) {
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return st, fmt.Errorf("load state %s: %w", cfg.StateFile, err)
	}
	if st.AgentID != "" {
		if st.AgentToken != "" {
			cli.SetToken(st.AgentToken)
			log.Printf("resuming as agent %s (per-agent credential)", st.AgentID)
		} else {
			cli.SetToken(cfg.Token)
			log.Printf("resuming as agent %s (legacy shared AGENT_TOKEN)", st.AgentID)
		}
		return st, nil
	}

	if cfg.EnrollmentToken == "" && cfg.Token == "" {
		return st, errors.New("not enrolled and no credential to enroll with: set ENROLLMENT_TOKEN (or the legacy AGENT_TOKEN)")
	}
	f := facts.Collect(agentVersion)
	req := nexus.EnrollRequest{
		Hostname:        f.Hostname,
		Hostgroup:       cfg.Hostgroup,
		MachineID:       f.MachineID,
		EnrollmentToken: cfg.EnrollmentToken,
	}
	ag, err := enrollWithRetry(cli, req, cfg.Once)
	if err != nil {
		return st, err
	}

	st.AgentID = ag.Identity()
	st.AgentToken = ag.AgentToken
	st.EnrollmentTokenID = ag.EnrollmentTokenID
	st.Hostgroup = ag.Hostgroup
	if ag.Environment != "" && st.Environment == nil {
		st.Environment = &state.Environment{Environment: ag.Environment, Monitored: true, UpdatedAt: time.Now().UTC()}
	}
	if err := state.Save(cfg.StateFile, st); err != nil {
		return st, fmt.Errorf("save state %s: %w", cfg.StateFile, err)
	}

	switch {
	case ag.AgentToken != "":
		cli.SetToken(ag.AgentToken)
	case cfg.EnrollmentToken != "" && cfg.Token == "":
		log.Printf("WARNING: Nexus issued no per-agent credential and AGENT_TOKEN is unset; later calls will be unauthenticated")
		cli.SetToken("")
	default:
		cli.SetToken(cfg.Token)
	}
	how := "legacy system key"
	if cfg.EnrollmentToken != "" {
		how = "enrollment token"
	}
	log.Printf("enrolled as agent %s (hostname=%s hostgroup=%s via %s, readopted=%v, per-agent credential=%v)",
		st.AgentID, ag.Hostname, ag.Hostgroup, how, ag.Readopted, ag.AgentToken != "")
	return st, nil
}

// enrollWithRetry enrolls, retrying with backoff (capped at 5 minutes) so a
// host that boots before Nexus is reachable, or before its token is valid,
// keeps trying instead of crash-looping. In one-shot mode it tries once.
func enrollWithRetry(cli *nexus.Client, req nexus.EnrollRequest, once bool) (nexus.Agent, error) {
	backoff := 5 * time.Second
	for {
		ag, err := cli.Enroll(req)
		if err == nil {
			return ag, nil
		}
		if nexus.IsUnauthorized(err) {
			if req.EnrollmentToken != "" {
				log.Printf("ERROR: Nexus refused the enrollment token (401): it is unknown, expired, revoked or used up. "+
					"Mint a new one and set ENROLLMENT_TOKEN. (%v)", err)
			} else {
				log.Printf("ERROR: Nexus refused AGENT_TOKEN for enrollment (401). (%v)", err)
			}
		} else {
			log.Printf("enroll failed: %v", err)
		}
		if once {
			return ag, fmt.Errorf("enroll: %w", err)
		}
		log.Printf("retrying enrollment in %s", backoff)
		time.Sleep(backoff)
		if backoff *= 2; backoff > 5*time.Minute {
			backoff = 5 * time.Minute
		}
	}
}

// sendFacts collects the full inventory and reports it.
func (a *agent) sendFacts() (string, error) {
	f := facts.CollectFull(agentVersion)
	if err := a.cli.Report(a.id, f); err != nil {
		return "", err
	}
	dns := "none"
	if f.DNSServer != nil {
		dns = fmt.Sprintf("%s %s running=%v zones=%d", f.DNSServer.Software, f.DNSServer.Version, f.DNSServer.Running, len(f.DNSServer.Zones))
	}
	return fmt.Sprintf("reported facts: os=%q kernel=%q arch=%q cpu=%d memMB=%d diskGB=%d uptime=%ds publicIp=%q "+
		"interfaces=%d listening=%d services=%d packages=%d dns=%s",
		f.OS, f.Kernel, f.Arch, f.CPUCores, f.MemoryMB, f.DiskGB, f.UptimeSeconds, f.PublicIP,
		len(f.Interfaces), len(f.Listening), len(f.Services), len(f.Packages), dns), nil
}

// periodicFacts ships a facts report unless an operator marked this machine
// not monitored (agent.environment monitored=false).
func (a *agent) periodicFacts() {
	st, err := state.Load(a.cfg.StateFile)
	if err != nil {
		log.Printf("load state: %v (reporting facts anyway)", err)
	}
	monitored := err != nil || st.Monitored()
	a.mu.Lock()
	changed := a.muted == monitored
	a.muted = !monitored
	a.mu.Unlock()
	if !monitored {
		if changed {
			log.Printf("not monitored: periodic facts reports paused (heartbeats and tasks continue)")
		}
		return
	}
	if changed {
		log.Printf("monitored again: periodic facts reports resumed")
	}
	if summary, err := a.sendFacts(); err != nil {
		a.auth.note("report facts", err)
	} else {
		log.Print(summary)
	}
}

// runCycle sends a heartbeat, polls for tasks, and executes each one.
func (a *agent) runCycle() {
	if err := a.cli.Heartbeat(a.id); err != nil {
		a.auth.note("heartbeat", err)
	}
	tasks, err := a.cli.PollTasks(a.id)
	if err != nil {
		a.auth.note("poll tasks", err)
		return
	}
	if len(tasks) == 0 {
		return
	}
	log.Printf("received %d task(s)", len(tasks))
	for _, t := range tasks {
		a.handleTask(t)
	}
}

// handleTask executes a task's plan, shipping a log line per step and reporting
// the terminal result with per-step outcomes. A failed critical step aborts
// the remaining steps, which are reported as skipped.
func (a *agent) handleTask(t nexus.Task) {
	log.Printf("task %s intent=%s steps=%d", t.TaskID, t.Intent, len(t.Plan.Steps))

	logs := make([]nexus.LogEntry, 0, len(t.Plan.Steps))
	results := make([]executor.StepResult, 0, len(t.Plan.Steps))
	opts := executor.Options{
		AllowDestructive: a.cfg.AllowDestructive,
		StateFile:        a.cfg.StateFile,
		SendFacts:        a.sendFacts,
	}

	for _, step := range t.Plan.Steps {
		r := executor.ExecuteStep(step, opts)
		results = append(results, r)
		level := "info"
		outcome := "ok"
		if r.Changed {
			outcome = "changed"
		} else if r.OK {
			outcome = "unchanged"
		}
		if !r.OK {
			level = "error"
			outcome = "failed"
		}
		logs = append(logs, nexus.LogEntry{
			Level:   level,
			Source:  "agent",
			Message: fmt.Sprintf("%s [%s]: %s", r.Action, outcome, firstNonEmpty(r.Output, r.Err, "ok")),
			TaskID:  t.TaskID,
			Metadata: map[string]any{
				"stepId":  r.StepID,
				"action":  r.Action,
				"changed": r.Changed,
			},
		})
		if !r.OK && step.Critical {
			break
		}
	}

	if err := a.cli.ShipLogs(a.id, logs); err != nil {
		a.auth.note("ship logs", err)
	}

	res := report.Build(t.Intent, t.Plan.Steps, results)
	if err := a.cli.SendResult(a.id, t.TaskID, res); err != nil {
		a.auth.note("report result", err)
	}
	log.Printf("task %s -> %s (exit %d)", t.TaskID, res.Status, res.ExitCode)
}

// authWatch logs call failures, and makes a refused credential (401) loud
// and actionable instead of one more line in a stream of retries.
type authWatch struct {
	stateFile string
	perAgent  bool

	mu   sync.Mutex
	last time.Time
}

const authAlarmEvery = 5 * time.Minute

func (w *authWatch) note(op string, err error) {
	if !nexus.IsUnauthorized(err) {
		log.Printf("%s: %v", op, err)
		return
	}
	w.mu.Lock()
	loud := time.Since(w.last) >= authAlarmEvery
	if loud {
		w.last = time.Now()
	}
	w.mu.Unlock()
	if !loud {
		log.Printf("%s: credential refused (401)", op)
		return
	}
	if w.perAgent {
		log.Printf("ERROR: Nexus refused this agent's credential (401) on %s. The per-agent token in %s was "+
			"revoked or rotated, or the agent was deleted. The agent keeps retrying but can do nothing until this is "+
			"fixed: stop it, remove %s, set a fresh ENROLLMENT_TOKEN (e.g. in /etc/linexus/agent.env) and start it "+
			"again — Nexus re-adopts the same agent id by machine id. (%v)", op, w.stateFile, w.stateFile, err)
	} else {
		log.Printf("ERROR: Nexus refused AGENT_TOKEN (401) on %s. Fix AGENT_TOKEN, or re-enroll with an "+
			"ENROLLMENT_TOKEN after removing %s. (%v)", op, w.stateFile, err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
