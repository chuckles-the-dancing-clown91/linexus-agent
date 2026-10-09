// Command rmm-agent is the Linexus infrastructure (RMM) agent. It runs on a
// managed host, enrolls with Nexus, reports facts and liveness, polls for
// tasks, executes their plan steps, and ships results and logs back — all
// through Nexus, the single service it talks to.
//
// This is distinct from the repo's Rust edge-telemetry scaffold (which serves
// the Dignifundus economy's solar/biodigester hardware). This binary manages
// conventional IT infrastructure for Daedalus IT.
//
// Every plan Nexus hands over is signed (Ed25519); the agent executes only
// plans whose signature verifies against the pinned Nexus key, issued for
// this agent, unexpired and never executed before.
package main

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/config"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/executor"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/facts"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/plansig"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/report"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/state"
)

const agentVersion = "0.3.0"

// agent is the running agent: its Nexus client, identity and configuration.
type agent struct {
	cli  *nexus.Client
	cfg  config.Config
	id   string
	auth *authWatch
	pin  plansig.Pin

	mu    sync.Mutex
	muted bool // last observed "not monitored" state, for logging transitions
}

func main() {
	cfg := config.Load()
	log.Printf("linexus rmm-agent v%s starting (nexus=%s once=%v allowDestructive=%v facts=%s)",
		agentVersion, cfg.NexusURL, cfg.Once, cfg.AllowDestructive, cfg.FactsInterval)
	if cfg.AllowUnsigned {
		log.Printf("WARNING: LINEXUS_ALLOW_UNSIGNED=1 — while no signing key is pinned this agent runs UNSIGNED plans " +
			"as root: anyone who can impersonate Nexus or steal its database can run commands here. Only for a Nexus " +
			"too old to sign plans; remove it as soon as Nexus signs.")
	}

	cli, err := newClient(cfg)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := identify(cli, cfg)
	if err != nil {
		log.Fatalf("identify: %v", err)
	}
	pin, err := plansig.ResolvePin(cfg.SigningPubKey, st.SigningKey)
	if err != nil {
		log.Fatalf("signing key: %v", err)
	}
	a := &agent{cli: cli, cfg: cfg, id: st.AgentID, pin: pin,
		auth: &authWatch{stateFile: cfg.StateFile, perAgent: st.AgentToken != ""}}
	a.logPin()
	a.checkAdvertisedKey()

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

// newClient checks NEXUS_URL's scheme and builds the client with the
// configured TLS trust and client certificate.
func newClient(cfg config.Config) (*nexus.Client, error) {
	if err := nexus.CheckURL(cfg.NexusURL, cfg.AllowInsecure); err != nil {
		return nil, err
	}
	tlsCfg, err := nexus.TLSOptions{
		CAFile:     cfg.CAFile,
		CAOnly:     cfg.CAOnly,
		ClientCert: cfg.ClientCert,
		ClientKey:  cfg.ClientKey,
	}.TLSConfig()
	if err != nil {
		return nil, err
	}
	if cfg.AllowInsecure && strings.HasPrefix(strings.ToLower(cfg.NexusURL), "http://") {
		log.Printf("WARNING: LINEXUS_ALLOW_INSECURE=1 — talking to Nexus over plain http; the agent credential travels unencrypted")
	}
	return nexus.NewTLS(cfg.NexusURL, cfg.Token, tlsCfg), nil
}

// identify resumes the persisted identity, or enrolls and persists a new one,
// and points the client at the right credential: the per-agent token from
// the state file, else (a legacy agent) AGENT_TOKEN.
func identify(cli *nexus.Client, cfg config.Config) (state.State, error) {
	st, err := state.Load(cfg.StateFile)
	if err != nil {
		return st, fmt.Errorf("load state %s: %w", cfg.StateFile, err)
	}
	if cfg.SigningPubKey != "" {
		if _, err := plansig.ParsePublicKey(cfg.SigningPubKey); err != nil {
			return st, fmt.Errorf("LINEXUS_SIGNING_PUBKEY: %w", err)
		}
	}
	if st.AgentID != "" {
		if cfg.SigningPubKey != "" && st.SigningKey != nil && !st.SigningKey.Equal(plansig.Key{PublicKey: cfg.SigningPubKey}) {
			log.Printf("note: LINEXUS_SIGNING_PUBKEY differs from the signing key saved at enrollment; the environment wins")
		}
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
	// With a key pinned in the environment, refuse a Nexus that advertises a
	// different one before spending the one-time enrollment token on it.
	if cfg.SigningPubKey != "" {
		if k, err := cli.SigningKey(); err == nil {
			if _, err := plansig.CheckEnrollmentKey(cfg.SigningPubKey, &k); err != nil {
				return st, err
			}
		} else if !nexus.IsNotFound(err) {
			log.Printf("could not fetch Nexus's signing key before enrolling (checking the enrollment answer instead): %v", err)
		}
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
	key, err := plansig.CheckEnrollmentKey(cfg.SigningPubKey, ag.SigningKey)
	if err != nil {
		// Nothing is saved, so this machine stays unenrolled here; the
		// enrollment token may be spent and the agent record left on Nexus.
		return st, fmt.Errorf("enrollment answer refused: %w", err)
	}

	st.AgentID = ag.Identity()
	st.AgentToken = ag.AgentToken
	st.EnrollmentTokenID = ag.EnrollmentTokenID
	st.Hostgroup = ag.Hostgroup
	st.SigningKey = key
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
	log.Printf("enrolled as agent %s (hostname=%s hostgroup=%s via %s, readopted=%v, per-agent credential=%v, signing key=%v)",
		st.AgentID, ag.Hostname, ag.Hostgroup, how, ag.Readopted, ag.AgentToken != "", key != nil)
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

// logPin says at startup which key plans are verified against.
func (a *agent) logPin() {
	switch {
	case a.pin.Pinned():
		id := a.pin.KeyID
		if id == "" {
			id = "?"
		}
		log.Printf("plan signing: verifying against pinned key %s (keyId %s, from %s)",
			plansig.Fingerprint(a.pin.Key), id, map[string]string{"env": "LINEXUS_SIGNING_PUBKEY", "state": "the state file"}[a.pin.Source])
	case a.cfg.AllowUnsigned:
		log.Printf("WARNING: plan signing: no key pinned and LINEXUS_ALLOW_UNSIGNED=1 — unsigned plans WILL run")
	default:
		log.Printf("ERROR: plan signing: no key pinned — every task will be refused. Set LINEXUS_SIGNING_PUBKEY " +
			"(GET <nexus>/api/v1/signing-key, checked out of band) in /etc/linexus/agent.env and restart")
	}
}

// checkAdvertisedKey compares the key Nexus currently advertises with the
// pinned one, to explain refusals ahead of time (a rotated key, an old
// Nexus). It never changes the pin.
func (a *agent) checkAdvertisedKey() {
	k, err := a.cli.SigningKey()
	if err != nil {
		if nexus.IsNotFound(err) && a.pin.Pinned() {
			log.Printf("WARNING: Nexus does not advertise a signing key (GET /api/v1/signing-key -> 404): it cannot sign " +
				"plans, and with a key pinned every task will be refused")
		} else if !nexus.IsNotFound(err) {
			log.Printf("could not fetch Nexus's advertised signing key: %v", err)
		}
		return
	}
	pub, err := k.Parse()
	if err != nil {
		log.Printf("WARNING: Nexus advertises an unusable signing key: %v", err)
		return
	}
	switch {
	case !a.pin.Pinned():
		log.Printf("Nexus advertises signing key %s (keyId %q). After checking it out of band, pin it with "+
			"LINEXUS_SIGNING_PUBKEY=%s", plansig.Fingerprint(pub), k.KeyID, k.PublicKey)
	case !a.pin.Key.Equal(pub):
		log.Printf("WARNING: Nexus now advertises signing key %s (keyId %q) but this agent pins %s: tasks signed with "+
			"the new key will fail verification until LINEXUS_SIGNING_PUBKEY is updated to it",
			plansig.Fingerprint(pub), k.KeyID, plansig.Fingerprint(a.pin.Key))
	}
}

// verifier builds the plan verifier for the current pin.
func (a *agent) verifier(executed func(string) bool) *plansig.Verifier {
	return &plansig.Verifier{
		Key:           a.pin.Key,
		KeyID:         a.pin.KeyID,
		AgentID:       a.id,
		AllowUnsigned: a.cfg.AllowUnsigned,
		Executed:      executed,
	}
}

// authorize verifies a task's signed envelope and returns the plan to run —
// the signed payload's, never the unsigned copy — recording the task as
// executed first so it can never run twice. The error is the reason for
// refusing the task.
func (a *agent) authorize(t nexus.Task) (string, nexus.Plan, error) {
	st, err := state.Load(a.cfg.StateFile)
	if err != nil {
		return "", nexus.Plan{}, localError{fmt.Errorf("cannot read the executed-task record in %s: %v", a.cfg.StateFile, err)}
	}
	intent, plan := t.Intent, t.Plan
	p, err := a.verifier(st.HasExecuted).Verify(t.TaskID, t.Envelope)
	switch {
	case errors.Is(err, plansig.ErrUnsignedAllowed):
		log.Printf("WARNING: task %s is UNSIGNED and runs only because LINEXUS_ALLOW_UNSIGNED=1", t.TaskID)
	case err != nil:
		return "", nexus.Plan{}, err
	default:
		plan, err = nexus.DecodePlan(p.Plan)
		if err != nil {
			return "", nexus.Plan{}, fmt.Errorf("signed plan is malformed: %v", err)
		}
		intent = p.Intent
	}
	if _, err := state.Update(a.cfg.StateFile, func(s *state.State) { s.MarkExecuted(t.TaskID) }); err != nil {
		return "", nexus.Plan{}, localError{fmt.Errorf("cannot record the task as executed (replay protection) in %s: %v", a.cfg.StateFile, err)}
	}
	return intent, plan, nil
}

// localError is a refusal caused by the agent itself (its replay record could
// not be read or written), not by the task's signature.
type localError struct{ error }

// refuse reports a task the agent will not run as failed, with the reason,
// through the normal log and result routes so the operator sees it.
func (a *agent) refuse(t nexus.Task, reason string) {
	log.Printf("ERROR: task %s refused: %s", t.TaskID, reason)
	entry := nexus.LogEntry{Level: "error", Source: "agent", Message: "task refused: " + reason, TaskID: t.TaskID}
	if err := a.cli.ShipLogs(a.id, []nexus.LogEntry{entry}); err != nil {
		a.auth.note("ship logs", err)
	}
	res := report.Refused(t.Intent, t.Plan.Steps, reason)
	if err := a.cli.SendResult(a.id, t.TaskID, res); err != nil {
		a.auth.note("report result", err)
	}
}

// handleTask verifies a task, executes its signed plan, ships a log line per
// step and reports the terminal result with per-step outcomes. A failed
// critical step aborts the remaining steps, which are reported as skipped. A
// task that fails verification is not executed and is reported as failed.
func (a *agent) handleTask(t nexus.Task) {
	intent, plan, err := a.authorize(t)
	if err != nil {
		reason := "signature verification failed: " + err.Error()
		var le localError
		if errors.As(err, &le) {
			reason = "not executed: " + err.Error()
		}
		a.refuse(t, reason)
		return
	}
	t.Intent, t.Plan = intent, plan
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
