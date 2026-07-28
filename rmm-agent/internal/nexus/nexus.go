// Package nexus is the agent's HTTP client for the Nexus gateway. Nexus is the
// only service the agent talks to: it enrolls, reports facts, heartbeats, polls
// for tasks, reports results, and ships logs — all through this one surface.
package nexus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	token   string
	hc      *http.Client
}

// New builds a client. The transport disables proxying: the agent reaches Nexus
// directly (LAN/VPN), never through an outbound web proxy.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		hc: &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{Proxy: nil},
		},
	}
}

// Agent is the record Nexus returns on enroll.
type Agent struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	State    string `json:"state"`
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

// Task is one unit of work handed to the agent.
type Task struct {
	TaskID string `json:"taskId"`
	Intent string `json:"intent"`
	Status string `json:"status"`
	Plan   Plan   `json:"plan"`
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("nexus %s %s -> %d: %s", method, path, resp.StatusCode, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// Enroll registers this machine and returns its assigned record.
func (c *Client) Enroll(hostname, hostgroup, manifest string) (Agent, error) {
	body := map[string]any{"hostname": hostname}
	if hostgroup != "" {
		body["hostgroup"] = hostgroup
	}
	if manifest != "" {
		body["capabilityManifest"] = manifest
	}
	var a Agent
	err := c.do(http.MethodPost, "/api/v1/agents/enroll", body, &a)
	return a, err
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

// ReportResult records a terminal task result. `status` is "success" or "failed".
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
