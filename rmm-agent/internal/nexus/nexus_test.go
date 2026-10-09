package nexus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendResultPostsFullBody(t *testing.T) {
	var gotPath, gotAuth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = w.Write([]byte(`{"taskId":"t1","status":"failed"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "k")
	err := c.SendResult("a1", "t1", Result{
		Status:   "failed",
		Error:    "exit status 2",
		Message:  "executed run_command (1 steps)",
		ExitCode: 2,
		Output:   "==> command.run [failed]\nnope",
		Steps: []StepReport{
			{ID: "s1", Action: "command.run", Status: "failed", Changed: true, Output: "nope", Error: "exit status 2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/agents/a1/tasks/t1/result" || gotAuth != "Bearer k" {
		t.Errorf("path=%q auth=%q", gotPath, gotAuth)
	}
	for k, want := range map[string]any{
		"status": "failed", "error": "exit status 2", "message": "executed run_command (1 steps)",
		"exitCode": float64(2), "output": "==> command.run [failed]\nnope",
	} {
		if got[k] != want {
			t.Errorf("%s = %#v, want %#v", k, got[k], want)
		}
	}
	steps, _ := got["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("steps = %#v", got["steps"])
	}
	step := steps[0].(map[string]any)
	for k, want := range map[string]any{
		"id": "s1", "action": "command.run", "status": "failed", "changed": true, "output": "nope", "error": "exit status 2",
	} {
		if step[k] != want {
			t.Errorf("step %s = %#v, want %#v", k, step[k], want)
		}
	}
}

func TestSendResultAlwaysSendsStepsArray(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()

	if err := New(srv.URL, "").SendResult("a", "t", Result{Status: "success"}); err != nil {
		t.Fatal(err)
	}
	if s, ok := got["steps"].([]any); !ok || len(s) != 0 {
		t.Errorf("steps = %#v, want []", got["steps"])
	}
	if got["exitCode"] != float64(0) {
		t.Errorf("exitCode = %#v, want 0", got["exitCode"])
	}
}

func TestEnrollWithTokenSendsNoBearer(t *testing.T) {
	var gotAuth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"agentId":"a9","id":"a9","hostname":"h","hostgroup":"acme","environment":"production","agentToken":"nxa_t","enrollmentTokenId":"e1","readopted":false}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "system-key")
	a, err := c.Enroll(EnrollRequest{Hostname: "h", Hostgroup: "x", MachineID: "m1", EnrollmentToken: "nxe_abc"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("bearer sent with an enrollment token: %q", gotAuth)
	}
	for k, want := range map[string]any{"hostname": "h", "hostgroup": "x", "machineId": "m1", "enrollmentToken": "nxe_abc"} {
		if got[k] != want {
			t.Errorf("%s = %#v, want %#v", k, got[k], want)
		}
	}
	if a.Identity() != "a9" || a.AgentToken != "nxa_t" || a.EnrollmentTokenID != "e1" {
		t.Errorf("agent = %+v", a)
	}
}

func TestLegacyEnrollUsesBearerAndOldAnswer(t *testing.T) {
	var gotAuth string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"id":"old-1","hostname":"h","state":"healthy"}`))
	}))
	defer srv.Close()

	a, err := New(srv.URL, "system-key").Enroll(EnrollRequest{Hostname: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer system-key" {
		t.Errorf("auth = %q", gotAuth)
	}
	if _, ok := got["enrollmentToken"]; ok {
		t.Error("empty enrollmentToken should be omitted")
	}
	if a.Identity() != "old-1" || a.AgentToken != "" {
		t.Errorf("agent = %+v", a)
	}
}

func TestUnauthorizedIsDetectable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "nxa_old")
	err := c.Heartbeat("a1")
	if !IsUnauthorized(err) {
		t.Fatalf("err = %v, want unauthorized", err)
	}
	c.SetToken("nxa_new")
	if c.bearer() != "nxa_new" {
		t.Error("SetToken did not take")
	}
}

func TestPollDecodesEnvelopeAndEnrollDecodesSigningKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/a1/tasks":
			_, _ = w.Write([]byte(`[{"taskId":"t1","intent":"x","status":"planned","plan":{"steps":[]},
				"envelope":{"alg":"ed25519","keyId":"k1","payload":"cA==","signature":"cw=="}}]`))
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"agentId":"a1","agentToken":"nxa_t","signingKey":{"alg":"ed25519","keyId":"k1","publicKey":"AAAA"}}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	tasks, err := c.PollTasks("a1")
	if err != nil || len(tasks) != 1 || tasks[0].Envelope == nil || tasks[0].Envelope.KeyID != "k1" || tasks[0].Envelope.Payload != "cA==" {
		t.Fatalf("tasks = %+v err=%v", tasks, err)
	}
	a, err := c.Enroll(EnrollRequest{Hostname: "h", EnrollmentToken: "nxe"})
	if err != nil || a.SigningKey == nil || a.SigningKey.KeyID != "k1" || a.SigningKey.PublicKey != "AAAA" {
		t.Fatalf("agent = %+v err=%v", a, err)
	}
}

func TestDecodePlanObjectOrSteps(t *testing.T) {
	p, err := DecodePlan([]byte(`{"task_id":"t1","intent":"i","steps":[{"id":"s1","action":"command.run","critical":true}]}`))
	if err != nil || p.TaskID != "t1" || len(p.Steps) != 1 || !p.Steps[0].Critical {
		t.Fatalf("object: %+v %v", p, err)
	}
	p, err = DecodePlan([]byte(` [{"id":"s1","action":"agent.facts"}]`))
	if err != nil || len(p.Steps) != 1 || p.Steps[0].Action != "agent.facts" {
		t.Fatalf("array: %+v %v", p, err)
	}
}
