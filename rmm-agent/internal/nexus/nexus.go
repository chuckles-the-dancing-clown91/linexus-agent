// Package nexus is the agent's HTTP client for the Nexus gateway. Nexus is the
// only service the agent talks to: it enrolls, reports facts, heartbeats, polls
// for tasks, reports results, and ships logs — all through this one surface.
package nexus

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/plansig"
)

type Client struct {
	baseURL string
	hc      *http.Client

	mu    sync.RWMutex
	token string
}

// New builds a client with Go's default TLS settings (system roots, no
// client certificate). The transport disables proxying: the agent reaches
// Nexus directly (LAN/VPN), never through an outbound web proxy.
func New(baseURL, token string) *Client {
	return NewTLS(baseURL, token, nil)
}

// NewTLS builds a client that verifies Nexus (and presents a client
// certificate) per tlsCfg; nil means Go's defaults.
func NewTLS(baseURL, token string, tlsCfg *tls.Config) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		hc: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil,
				TLSClientConfig:     tlsCfg,
				TLSHandshakeTimeout: 10 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
}

// SetToken replaces the bearer presented on every later call (the per-agent
// credential once enrolled).
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

func (c *Client) bearer() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token
}

// HTTPError is a non-2xx answer from Nexus.
type HTTPError struct {
	Method, Path string
	Status       int
	Body         string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("nexus %s %s -> %d: %s", e.Method, e.Path, e.Status, e.Body)
}

// IsUnauthorized reports whether err is Nexus refusing the credential (401):
// a revoked or rotated agent token, a deleted agent, or a spent enrollment
// token.
func IsUnauthorized(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusUnauthorized
}

// EnrollRequest is the body of POST /api/v1/agents/enroll. With an
// EnrollmentToken the call carries no bearer; without one it is the legacy
// path, authenticated by a system key in the bearer.
type EnrollRequest struct {
	Hostname        string `json:"hostname"`
	Hostgroup       string `json:"hostgroup,omitempty"`
	MachineID       string `json:"machineId,omitempty"`
	EnrollmentToken string `json:"enrollmentToken,omitempty"`
}

// Agent is the record Nexus returns on enroll. A Nexus that issues per-agent
// credentials answers agentId + agentToken; an older one answers only id.
type Agent struct {
	AgentID           string `json:"agentId"`
	ID                string `json:"id"`
	Hostname          string `json:"hostname"`
	Hostgroup         string `json:"hostgroup"`
	Environment       string `json:"environment"`
	State             string `json:"state"`
	AgentToken        string `json:"agentToken"`
	EnrollmentTokenID string `json:"enrollmentTokenId"`
	Readopted         bool   `json:"readopted"`
	// SigningKey is the public key Nexus signs plans with; absent from an
	// older Nexus.
	SigningKey *plansig.Key `json:"signingKey,omitempty"`
}

// Identity is the agent id, whichever field carried it.
func (a Agent) Identity() string {
	if a.AgentID != "" {
		return a.AgentID
	}
	return a.ID
}

// Step is one executable action in a plan.
type Step struct {
	ID       string            `json:"id"`
	Action   string            `json:"action"`
	Params   map[string]string `json:"params"`
	Critical bool              `json:"critical"`
}

// Plan is the orchestrator's TransactionPlan.
type Plan struct {
	TaskID       string   `json:"task_id"`
	Intent       string   `json:"intent"`
	Targets      []string `json:"targets"`
	AutoRollback bool     `json:"auto_rollback"`
	Steps        []Step   `json:"steps"`
}

// Task is one unit of work handed to the agent. Plan is the unsigned copy,
// kept for display and for an old Nexus; a signing Nexus adds Envelope, whose
// payload carries the plan the agent actually executes.
type Task struct {
	TaskID   string            `json:"taskId"`
	Intent   string            `json:"intent"`
	Status   string            `json:"status"`
	Plan     Plan              `json:"plan"`
	Envelope *plansig.Envelope `json:"envelope,omitempty"`
}

// DecodePlan parses a plan as carried in a signed payload: the
// TransactionPlan object, or (tolerated) a bare array of steps.
func DecodePlan(raw json.RawMessage) (Plan, error) {
	var p Plan
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		err := json.Unmarshal(trimmed, &p.Steps)
		return p, err
	}
	err := json.Unmarshal(trimmed, &p)
	return p, err
}

// LogEntry is one journal line shipped back through Nexus to the Logger.
type LogEntry struct {
	Level    string         `json:"level,omitempty"`
	Source   string         `json:"source,omitempty"`
	Message  string         `json:"message"`
	TaskID   string         `json:"taskId,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

func (c *Client) do(method, path string, body, out any) error {
	return c.doAuth(method, path, c.bearer(), body, out)
}

func (c *Client) doAuth(method, path, token string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Enroll registers this machine and returns its assigned record. With an
// enrollment token in the request no bearer is sent (the token is the
// credential); otherwise the client's current token (a system key) is.
func (c *Client) Enroll(req EnrollRequest) (Agent, error) {
	token := c.bearer()
	if req.EnrollmentToken != "" {
		token = ""
	}
	var a Agent
	err := c.doAuth(http.MethodPost, "/api/v1/agents/enroll", token, req, &a)
	if err == nil && a.Identity() == "" {
		err = fmt.Errorf("nexus enroll: answer carried no agent id")
	}
	return a, err
}

// SigningKey fetches the public key Nexus signs plans with
// (GET /api/v1/signing-key, no auth). It is informational: the agent pins a
// key from its environment or its enrollment, never from this call.
func (c *Client) SigningKey() (plansig.Key, error) {
	var k plansig.Key
	err := c.doAuth(http.MethodGet, "/api/v1/signing-key", "", nil, &k)
	return k, err
}

// IsNotFound reports whether err is a 404 from Nexus.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// Report submits a facts payload (also a heartbeat).
func (c *Client) Report(id string, facts any) error {
	return c.do(http.MethodPost, "/api/v1/agents/"+id+"/report", facts, nil)
}

// Heartbeat sends a liveness signal.
func (c *Client) Heartbeat(id string) error {
	return c.do(http.MethodPost, "/api/v1/agents/"+id+"/heartbeat", nil, nil)
}

// PollTasks fetches tasks awaiting this agent.
func (c *Client) PollTasks(id string) ([]Task, error) {
	var tasks []Task
	err := c.do(http.MethodGet, "/api/v1/agents/"+id+"/tasks", nil, &tasks)
	return tasks, err
}

// StepReport is one plan step's outcome inside a Result. Status is "success",
// "failed", or "skipped" (a step never run because an earlier critical step
// failed).
type StepReport struct {
	ID      string `json:"id,omitempty"`
	Action  string `json:"action"`
	Status  string `json:"status"`
	Changed bool   `json:"changed"`
	Output  string `json:"output"`
	Error   string `json:"error"`
}

// Result is the body of a task result report. Status, Error and Message are
// the original protocol; ExitCode, Output and Steps were added later and are
// optional on the Nexus side, so an older Nexus simply ignores them.
type Result struct {
	Status   string       `json:"status"` // "success" or "failed"
	Error    string       `json:"error,omitempty"`
	Message  string       `json:"message,omitempty"`
	ExitCode int          `json:"exitCode"`
	Output   string       `json:"output"`
	Steps    []StepReport `json:"steps"`
}

// SendResult records a terminal task result with its exit code, combined
// output and per-step results.
func (c *Client) SendResult(id, taskID string, r Result) error {
	if r.Steps == nil {
		r.Steps = []StepReport{}
	}
	return c.do(http.MethodPost, "/api/v1/agents/"+id+"/tasks/"+taskID+"/result", r, nil)
}

// ReportResult records a terminal task result in the original, minimal form
// (no exit code, output or steps). `status` is "success" or "failed".
// Prefer SendResult.
func (c *Client) ReportResult(id, taskID, status, errMsg, message string) error {
	body := map[string]any{"status": status}
	if errMsg != "" {
		body["error"] = errMsg
	}
	if message != "" {
		body["message"] = message
	}
	return c.do(http.MethodPost, "/api/v1/agents/"+id+"/tasks/"+taskID+"/result", body, nil)
}

// ShipLogs forwards journal lines to Nexus (which relays them to the Logger).
func (c *Client) ShipLogs(id string, entries []LogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	return c.do(http.MethodPost, "/api/v1/agents/"+id+"/logs", entries, nil)
}
