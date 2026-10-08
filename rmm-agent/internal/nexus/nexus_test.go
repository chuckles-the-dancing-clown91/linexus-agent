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
