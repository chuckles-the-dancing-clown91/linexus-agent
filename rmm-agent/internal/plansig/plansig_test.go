package plansig

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

type fixture struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pub, priv}
}

func (f fixture) pubB64() string { return base64.StdEncoding.EncodeToString(f.pub) }

// payload builds the signed payload JSON for a task, as Nexus would.
func payload(taskID, agentID string, expires time.Time) map[string]any {
	return map[string]any{
		"v":       1,
		"taskId":  taskID,
		"agentId": agentID,
		"intent":  "run_command",
		"plan": map[string]any{
			"task_id": taskID, "intent": "run_command", "targets": []string{agentID},
			"steps": []any{map[string]any{"id": "s1", "action": "command.run", "params": map[string]string{"command": "true"}, "critical": true}},
		},
		"issuedAt":  now.Add(-time.Minute).Format(time.RFC3339),
		"expiresAt": expires.Format(time.RFC3339),
		"nonce":     "0011223344556677",
	}
}

func (f fixture) envelope(t *testing.T, p map[string]any) *Envelope {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return &Envelope{
		Alg:       "ed25519",
		KeyID:     "k1",
		Payload:   base64.StdEncoding.EncodeToString(b),
		Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(f.priv, b)),
	}
}

func (f fixture) verifier(executed ...string) *Verifier {
	return &Verifier{
		Key:     f.pub,
		KeyID:   "k1",
		AgentID: "agent-1",
		Executed: func(id string) bool {
			for _, e := range executed {
				if e == id {
					return true
				}
			}
			return false
		},
		Now: func() time.Time { return now },
	}
}

func wantErr(t *testing.T, err error, substr string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), substr) {
		t.Fatalf("err = %v, want one containing %q", err, substr)
	}
}

func TestVerifyValid(t *testing.T) {
	f := newFixture(t)
	p, err := f.verifier().Verify("task-1", f.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute))))
	if err != nil {
		t.Fatal(err)
	}
	if p.TaskID != "task-1" || p.Intent != "run_command" || p.Nonce == "" {
		t.Errorf("payload = %+v", p)
	}
	var plan struct {
		Steps []struct{ ID, Action string } `json:"steps"`
	}
	if err := json.Unmarshal(p.Plan, &plan); err != nil || len(plan.Steps) != 1 || plan.Steps[0].Action != "command.run" {
		t.Errorf("plan = %s (%v)", p.Plan, err)
	}
}

func TestVerifyBadSignature(t *testing.T) {
	f := newFixture(t)
	env := f.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute)))
	// Tamper with the payload after signing: swap in another command.
	b, _ := base64.StdEncoding.DecodeString(env.Payload)
	b = []byte(strings.Replace(string(b), `"command":"true"`, `"command":"rm -rf /"`, 1))
	env.Payload = base64.StdEncoding.EncodeToString(b)
	_, err := f.verifier().Verify("task-1", env)
	wantErr(t, err, "bad signature")

	// Signed by another key (e.g. a rotated Nexus key).
	other := newFixture(t)
	env = other.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute)))
	env.KeyID = "k2"
	_, err = f.verifier().Verify("task-1", env)
	wantErr(t, err, `signed with key "k2"`)

	// Malformed signature.
	env = f.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute)))
	env.Signature = base64.StdEncoding.EncodeToString([]byte("short"))
	_, err = f.verifier().Verify("task-1", env)
	wantErr(t, err, "signature is 5 bytes")

	env = f.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute)))
	env.Alg = "rsa"
	_, err = f.verifier().Verify("task-1", env)
	wantErr(t, err, "unsupported algorithm")
}

func TestVerifyWrongAgent(t *testing.T) {
	f := newFixture(t)
	_, err := f.verifier().Verify("task-1", f.envelope(t, payload("task-1", "agent-2", now.Add(10*time.Minute))))
	wantErr(t, err, `issued for agent "agent-2"`)
}

func TestVerifyTaskIDMismatch(t *testing.T) {
	f := newFixture(t)
	// A valid envelope for task-1 replayed under another task id.
	_, err := f.verifier().Verify("task-2", f.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute))))
	wantErr(t, err, "does not match the delivered task")
}

func TestVerifyExpired(t *testing.T) {
	f := newFixture(t)
	_, err := f.verifier().Verify("task-1", f.envelope(t, payload("task-1", "agent-1", now.Add(-6*time.Minute))))
	wantErr(t, err, "expired")
	// Within the 5 minute skew allowance it still runs.
	if _, err := f.verifier().Verify("task-1", f.envelope(t, payload("task-1", "agent-1", now.Add(-4*time.Minute)))); err != nil {
		t.Fatalf("within skew: %v", err)
	}
	p := payload("task-1", "agent-1", now)
	delete(p, "expiresAt")
	_, err = f.verifier().Verify("task-1", f.envelope(t, p))
	wantErr(t, err, "no expiry")
}

func TestVerifyVersion(t *testing.T) {
	f := newFixture(t)
	p := payload("task-1", "agent-1", now.Add(time.Minute))
	p["v"] = 2
	_, err := f.verifier().Verify("task-1", f.envelope(t, p))
	wantErr(t, err, "unsupported payload version 2")
}

func TestVerifyReplay(t *testing.T) {
	f := newFixture(t)
	_, err := f.verifier("task-1").Verify("task-1", f.envelope(t, payload("task-1", "agent-1", now.Add(10*time.Minute))))
	wantErr(t, err, "already executed (replay)")
}

func TestVerifyUnsigned(t *testing.T) {
	f := newFixture(t)

	// A key is pinned: unsigned is refused, even with AllowUnsigned.
	v := f.verifier()
	v.AllowUnsigned = true
	_, err := v.Verify("task-1", nil)
	wantErr(t, err, "unsigned but a signing key is pinned")

	// No key pinned, unsigned not allowed.
	v = &Verifier{AgentID: "agent-1"}
	_, err = v.Verify("task-1", nil)
	wantErr(t, err, "no signing key is pinned")

	// No key pinned, LINEXUS_ALLOW_UNSIGNED=1: the caller may run the plain plan.
	v.AllowUnsigned = true
	p, err := v.Verify("task-1", nil)
	if !errors.Is(err, ErrUnsignedAllowed) || p != nil {
		t.Fatalf("p=%v err=%v, want ErrUnsignedAllowed", p, err)
	}

	// No key pinned but the task is signed: nothing to verify it with.
	_, err = v.Verify("task-1", f.envelope(t, payload("task-1", "agent-1", now.Add(time.Minute))))
	wantErr(t, err, "no signing key is pinned to verify it")
}

func TestResolvePinPrecedence(t *testing.T) {
	envKey, stateKey := newFixture(t), newFixture(t)
	saved := &Key{Alg: "ed25519", KeyID: "ks", PublicKey: stateKey.pubB64()}

	p, err := ResolvePin(envKey.pubB64(), saved)
	if err != nil || p.Source != "env" || !p.Key.Equal(envKey.pub) || p.KeyID != "" {
		t.Fatalf("env over state: %+v %v", p, err)
	}
	p, err = ResolvePin("", saved)
	if err != nil || p.Source != "state" || !p.Key.Equal(stateKey.pub) || p.KeyID != "ks" {
		t.Fatalf("state: %+v %v", p, err)
	}
	p, err = ResolvePin(stateKey.pubB64(), saved)
	if err != nil || p.Source != "env" || p.KeyID != "ks" {
		t.Fatalf("env equal to state keeps the key id: %+v %v", p, err)
	}
	p, err = ResolvePin("", nil)
	if err != nil || p.Pinned() {
		t.Fatalf("none: %+v %v", p, err)
	}
	if _, err := ResolvePin("not-base64!!", nil); err == nil {
		t.Fatal("bad env key accepted")
	}
	if _, err := ResolvePin(base64.StdEncoding.EncodeToString([]byte("short")), nil); err == nil {
		t.Fatal("short env key accepted")
	}
}

func TestCheckEnrollmentKey(t *testing.T) {
	nexusKey, pinned := newFixture(t), newFixture(t)
	got := &Key{Alg: "ed25519", KeyID: "k1", PublicKey: nexusKey.pubB64()}

	k, err := CheckEnrollmentKey("", got)
	if err != nil || k == nil || k.KeyID != "k1" || k.PublicKey != nexusKey.pubB64() {
		t.Fatalf("no env pin: saves the advertised key, got %+v %v", k, err)
	}
	if k, err := CheckEnrollmentKey(nexusKey.pubB64(), got); err != nil || k == nil {
		t.Fatalf("matching env pin: %+v %v", k, err)
	}
	_, err = CheckEnrollmentKey(pinned.pubB64(), got)
	wantErr(t, err, "refusing to enroll")

	if k, err := CheckEnrollmentKey(pinned.pubB64(), nil); err != nil || k != nil {
		t.Fatalf("old Nexus (no key): %+v %v", k, err)
	}
	_, err = CheckEnrollmentKey("", &Key{Alg: "rsa", PublicKey: nexusKey.pubB64()})
	wantErr(t, err, "unsupported signing key algorithm")
}
