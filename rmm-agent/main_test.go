package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/config"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/nexus"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/plansig"
	"github.com/chuckles-the-dancing-clown91/linexus-agent/rmm-agent/internal/state"
)

// fakeNexus records what the agent posts and answers enrollment and the
// signing-key endpoint.
type fakeNexus struct {
	mu         sync.Mutex
	results    map[string]nexus.Result
	logs       []nexus.LogEntry
	advertised *plansig.Key // GET /api/v1/signing-key; nil = 404 (old Nexus)
	enrollKey  *plansig.Key // signingKey in the enrollment answer
	enrolls    int
}

func (f *fakeNexus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/api/v1/signing-key":
		if f.advertised == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(f.advertised)
	case r.URL.Path == "/api/v1/agents/enroll":
		f.enrolls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"agentId": "agent-1", "agentToken": "nxa_t", "hostgroup": "acme", "signingKey": f.enrollKey})
	case strings.HasSuffix(r.URL.Path, "/logs"):
		var entries []nexus.LogEntry
		_ = json.NewDecoder(r.Body).Decode(&entries)
		f.logs = append(f.logs, entries...)
	case strings.HasSuffix(r.URL.Path, "/result"):
		var res nexus.Result
		_ = json.NewDecoder(r.Body).Decode(&res)
		parts := strings.Split(r.URL.Path, "/")
		f.results[parts[len(parts)-2]] = res
	}
}

func (f *fakeNexus) result(t *testing.T, taskID string) nexus.Result {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[taskID]
	if !ok {
		t.Fatalf("no result posted for %s", taskID)
	}
	delete(f.results, taskID)
	return r
}

type signer struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T) signer {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{pub, priv}
}

func (s signer) key(id string) *plansig.Key {
	return &plansig.Key{Alg: "ed25519", KeyID: id, PublicKey: base64.StdEncoding.EncodeToString(s.pub)}
}

// task builds a polled task whose unsigned plan says one thing and whose
// signed payload sets the agent's environment to `env`.
func (s signer) task(t *testing.T, taskID, agentID, env string) nexus.Task {
	t.Helper()
	plan := nexus.Plan{TaskID: taskID, Intent: "set_environment", Steps: []nexus.Step{
		{ID: "s1", Action: "agent.environment", Params: map[string]string{"environment": env, "monitored": "true"}, Critical: true},
	}}
	pb, _ := json.Marshal(plan)
	payload, _ := json.Marshal(map[string]any{
		"v": 1, "taskId": taskID, "agentId": agentID, "intent": "set_environment", "plan": json.RawMessage(pb),
		"issuedAt": time.Now().UTC().Format(time.RFC3339), "expiresAt": time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
		"nonce": "abcdef",
	})
	return nexus.Task{
		TaskID: taskID, Intent: "set_environment", Status: "planned",
		// The unsigned copy has been tampered with: it must never run.
		Plan: nexus.Plan{Steps: []nexus.Step{{ID: "x1", Action: "agent.environment", Params: map[string]string{"environment": "tampered"}}}},
		Envelope: &plansig.Envelope{
			Alg: "ed25519", KeyID: "k1",
			Payload:   base64.StdEncoding.EncodeToString(payload),
			Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, payload)),
		},
	}
}

func newTestAgent(t *testing.T, srvURL string, pin plansig.Pin, allowUnsigned bool) (*agent, string) {
	t.Helper()
	sf := filepath.Join(t.TempDir(), "state.json")
	if err := state.Save(sf, state.State{AgentID: "agent-1", AgentToken: "nxa_t"}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{NexusURL: srvURL, StateFile: sf, AllowUnsigned: allowUnsigned}
	return &agent{cli: nexus.New(srvURL, "nxa_t"), cfg: cfg, id: "agent-1", pin: pin,
		auth: &authWatch{stateFile: sf, perAgent: true}}, sf
}

func environment(t *testing.T, sf string) string {
	t.Helper()
	st, err := state.Load(sf)
	if err != nil {
		t.Fatal(err)
	}
	if st.Environment == nil {
		return ""
	}
	return st.Environment.Environment
}

func TestHandleTaskRunsOnlyTheSignedPlanOnce(t *testing.T) {
	fn := &fakeNexus{results: map[string]nexus.Result{}}
	srv := httptest.NewServer(fn)
	defer srv.Close()
	s := newSigner(t)
	a, sf := newTestAgent(t, srv.URL, plansig.Pin{Key: s.pub, KeyID: "k1", Source: "state"}, false)

	task := s.task(t, "task-1", "agent-1", "staging")
	a.handleTask(task)
	res := fn.result(t, "task-1")
	if res.Status != "success" || len(res.Steps) != 1 || res.Steps[0].ID != "s1" {
		t.Fatalf("result = %+v", res)
	}
	if got := environment(t, sf); got != "staging" {
		t.Fatalf("environment = %q: the signed plan should have run (and not the unsigned copy)", got)
	}
	if st, _ := state.Load(sf); !st.HasExecuted("task-1") {
		t.Fatal("task not recorded as executed")
	}

	// The same signed task delivered again is a replay.
	_, _ = state.Update(sf, func(st *state.State) { st.Environment = nil })
	a.handleTask(task)
	res = fn.result(t, "task-1")
	if res.Status != "failed" || !strings.HasPrefix(res.Error, "signature verification failed: ") || !strings.Contains(res.Error, "replay") {
		t.Fatalf("replay result = %+v", res)
	}
	if got := environment(t, sf); got != "" {
		t.Fatalf("replayed plan ran: environment = %q", got)
	}
}

func TestHandleTaskRefusesBadTasks(t *testing.T) {
	fn := &fakeNexus{results: map[string]nexus.Result{}}
	srv := httptest.NewServer(fn)
	defer srv.Close()
	s, other := newSigner(t), newSigner(t)
	a, sf := newTestAgent(t, srv.URL, plansig.Pin{Key: s.pub, KeyID: "k1", Source: "env"}, true)

	unsigned := s.task(t, "t-unsigned", "agent-1", "prod")
	unsigned.Envelope = nil
	expired := s.task(t, "t-expired", "agent-1", "prod")
	{
		b, _ := base64.StdEncoding.DecodeString(expired.Envelope.Payload)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		p["expiresAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		b, _ = json.Marshal(p)
		expired.Envelope.Payload = base64.StdEncoding.EncodeToString(b)
		expired.Envelope.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, b))
	}

	for id, tc := range map[string]struct {
		task nexus.Task
		want string
	}{
		"t-badsig":   {other.task(t, "t-badsig", "agent-1", "prod"), "bad signature"},
		"t-wrong":    {s.task(t, "t-wrong", "agent-2", "prod"), "issued for agent"},
		"t-expired":  {expired, "expired"},
		"t-unsigned": {unsigned, "unsigned but a signing key is pinned"},
	} {
		a.handleTask(tc.task)
		res := fn.result(t, id)
		if res.Status != "failed" || res.ExitCode != 1 || !strings.HasPrefix(res.Error, "signature verification failed: ") ||
			!strings.Contains(res.Error, tc.want) {
			t.Errorf("%s: result = %+v, want error containing %q", id, res, tc.want)
		}
		for _, st := range res.Steps {
			if st.Status != "skipped" {
				t.Errorf("%s: step %+v ran", id, st)
			}
		}
	}
	if got := environment(t, sf); got != "" {
		t.Fatalf("a refused plan ran: environment = %q", got)
	}
	fn.mu.Lock()
	defer fn.mu.Unlock()
	if len(fn.logs) != 4 || fn.logs[0].Level != "error" || !strings.HasPrefix(fn.logs[0].Message, "task refused: signature verification failed") {
		t.Errorf("logs = %+v", fn.logs)
	}
}

func TestHandleTaskUnsignedWithoutPin(t *testing.T) {
	fn := &fakeNexus{results: map[string]nexus.Result{}}
	srv := httptest.NewServer(fn)
	defer srv.Close()
	s := newSigner(t)
	task := s.task(t, "t1", "agent-1", "prod")
	task.Envelope = nil
	task.Plan = nexus.Plan{Steps: []nexus.Step{{ID: "u1", Action: "agent.environment", Params: map[string]string{"environment": "legacy"}}}}

	// Refused by default.
	a, sf := newTestAgent(t, srv.URL, plansig.Pin{}, false)
	a.handleTask(task)
	if res := fn.result(t, "t1"); res.Status != "failed" || !strings.Contains(res.Error, "no signing key is pinned") {
		t.Fatalf("result = %+v", res)
	}
	if environment(t, sf) != "" {
		t.Fatal("unsigned plan ran without LINEXUS_ALLOW_UNSIGNED")
	}

	// Allowed with LINEXUS_ALLOW_UNSIGNED=1 while nothing is pinned.
	a, sf = newTestAgent(t, srv.URL, plansig.Pin{}, true)
	a.handleTask(task)
	if res := fn.result(t, "t1"); res.Status != "success" {
		t.Fatalf("result = %+v", res)
	}
	if environment(t, sf) != "legacy" {
		t.Fatal("unsigned plan did not run with LINEXUS_ALLOW_UNSIGNED=1")
	}
}

func TestIdentifySavesEnrollmentKey(t *testing.T) {
	s := newSigner(t)
	fn := &fakeNexus{results: map[string]nexus.Result{}, advertised: s.key("k1"), enrollKey: s.key("k1")}
	srv := httptest.NewServer(fn)
	defer srv.Close()
	sf := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Config{NexusURL: srv.URL, StateFile: sf, EnrollmentToken: "nxe_x", Once: true}

	st, err := identify(nexus.New(srv.URL, ""), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.SigningKey == nil || st.SigningKey.KeyID != "k1" {
		t.Fatalf("state key = %+v", st.SigningKey)
	}
	saved, _ := state.Load(sf)
	pin, err := plansig.ResolvePin("", saved.SigningKey)
	if err != nil || pin.Source != "state" || !pin.Key.Equal(s.pub) {
		t.Fatalf("pin = %+v %v", pin, err)
	}
}

func TestIdentifyRefusesEnvPinMismatch(t *testing.T) {
	nexusKey, pinned := newSigner(t), newSigner(t)
	envPin := base64.StdEncoding.EncodeToString(pinned.pub)

	// Nexus advertises its key: refused before the enrollment token is spent.
	fn := &fakeNexus{results: map[string]nexus.Result{}, advertised: nexusKey.key("k1"), enrollKey: nexusKey.key("k1")}
	srv := httptest.NewServer(fn)
	defer srv.Close()
	sf := filepath.Join(t.TempDir(), "state.json")
	cfg := config.Config{NexusURL: srv.URL, StateFile: sf, EnrollmentToken: "nxe_x", Once: true, SigningPubKey: envPin}
	_, err := identify(nexus.New(srv.URL, ""), cfg)
	if err == nil || !strings.Contains(err.Error(), "refusing to enroll") {
		t.Fatalf("err = %v", err)
	}
	if fn.enrolls != 0 {
		t.Errorf("enrolled %d times despite the mismatch", fn.enrolls)
	}

	// No signing-key endpoint, but the enrollment answer carries the other key.
	fn.advertised = nil
	_, err = identify(nexus.New(srv.URL, ""), cfg)
	if err == nil || !strings.Contains(err.Error(), "refusing to enroll") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := state.Load(sf); st.AgentID != "" {
		t.Fatalf("state saved despite the mismatch: %+v", st)
	}

	// Matching pin enrolls.
	cfg.SigningPubKey = base64.StdEncoding.EncodeToString(nexusKey.pub)
	if _, err := identify(nexus.New(srv.URL, ""), cfg); err != nil {
		t.Fatal(err)
	}
}
