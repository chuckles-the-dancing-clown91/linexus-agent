// Package plansig verifies the Ed25519 signatures Nexus puts on the plans it
// hands an agent, so a task is executed only when Nexus — holding the private
// key — issued it for this agent, recently, and only once.
//
// Each polled task carries an envelope:
//
//	{"alg":"ed25519","keyId":"…","payload":"<base64>","signature":"<base64>"}
//
// The signature covers the exact decoded payload bytes, which are UTF-8 JSON:
//
//	{"v":1,"taskId":"<uuid>","agentId":"<agent id>","intent":"…","plan":{…},
//	 "issuedAt":"RFC3339","expiresAt":"RFC3339","nonce":"hex"}
//
// The agent executes payload.plan, never the unsigned plan beside it.
package plansig

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Alg is the only signature algorithm the agent accepts.
const Alg = "ed25519"

// DefaultSkew is how far the agent's clock may lag behind Nexus's before an
// unexpired plan is refused as expired.
const DefaultSkew = 5 * time.Minute

// PayloadVersion is the payload format the agent understands.
const PayloadVersion = 1

// Key is a Nexus signing public key as Nexus advertises it
// (GET /api/v1/signing-key, and "signingKey" in the enrollment answer).
type Key struct {
	Alg       string `json:"alg"`
	KeyID     string `json:"keyId,omitempty"`
	PublicKey string `json:"publicKey"` // base64 (std, padded), 32 bytes
}

// Parse decodes the public key, checking the algorithm and length.
func (k Key) Parse() (ed25519.PublicKey, error) {
	if k.Alg != "" && !strings.EqualFold(k.Alg, Alg) {
		return nil, fmt.Errorf("unsupported signing key algorithm %q (want %s)", k.Alg, Alg)
	}
	return ParsePublicKey(k.PublicKey)
}

// Equal reports whether two keys carry the same public key bytes.
func (k Key) Equal(o Key) bool {
	a, err1 := k.Parse()
	b, err2 := o.Parse()
	return err1 == nil && err2 == nil && a.Equal(b)
}

// ParsePublicKey decodes a base64 Ed25519 public key (standard alphabet;
// padding and URL-safe forms are tolerated).
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := decodeB64(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("signing public key is not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signing public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// Fingerprint is a short, log-friendly form of a public key.
func Fingerprint(pub ed25519.PublicKey) string {
	s := base64.StdEncoding.EncodeToString(pub)
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// Envelope is the signed form of a task's plan.
type Envelope struct {
	Alg       string `json:"alg"`
	KeyID     string `json:"keyId,omitempty"`
	Payload   string `json:"payload"`   // base64 of the payload bytes
	Signature string `json:"signature"` // base64 of the 64-byte signature
}

// Payload is the signed content.
type Payload struct {
	V         int             `json:"v"`
	TaskID    string          `json:"taskId"`
	AgentID   string          `json:"agentId"`
	Intent    string          `json:"intent"`
	Plan      json.RawMessage `json:"plan"`
	IssuedAt  time.Time       `json:"issuedAt"`
	ExpiresAt time.Time       `json:"expiresAt"`
	Nonce     string          `json:"nonce"`
}

// Verifier checks envelopes for one agent.
type Verifier struct {
	// Key is the pinned Nexus public key; nil when none is pinned.
	Key ed25519.PublicKey
	// KeyID is the pinned key's id when known (only used in error messages).
	KeyID string
	// AgentID is this agent's id; a payload for another agent is refused.
	AgentID string
	// AllowUnsigned lets unsigned tasks through, but only while no key is
	// pinned (an old Nexus that cannot sign).
	AllowUnsigned bool
	// Executed reports whether a task id was already executed (replay).
	Executed func(taskID string) bool
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Skew is the tolerated clock skew (DefaultSkew when zero).
	Skew time.Duration
}

// ErrUnsignedAllowed is returned (with a nil payload) when a task carries no
// envelope and unsigned tasks are allowed: the caller may run its plain plan.
var ErrUnsignedAllowed = errors.New("unsigned task allowed (no signing key pinned, LINEXUS_ALLOW_UNSIGNED=1)")

// Verify checks a task's envelope and returns its signed payload. taskID is
// the id the task was delivered under. A nil envelope yields
// ErrUnsignedAllowed when unsigned tasks may run, else an error. Any other
// error means the task must not be executed.
func (v *Verifier) Verify(taskID string, env *Envelope) (*Payload, error) {
	if env == nil {
		switch {
		case v.Key != nil:
			return nil, errors.New("task is unsigned but a signing key is pinned")
		case v.AllowUnsigned:
			return nil, ErrUnsignedAllowed
		default:
			return nil, errors.New("task is unsigned and no signing key is pinned " +
				"(set LINEXUS_SIGNING_PUBKEY, or LINEXUS_ALLOW_UNSIGNED=1 for a Nexus that cannot sign)")
		}
	}
	if v.Key == nil {
		return nil, errors.New("task is signed but no signing key is pinned to verify it (set LINEXUS_SIGNING_PUBKEY)")
	}
	if !strings.EqualFold(env.Alg, Alg) {
		return nil, fmt.Errorf("unsupported algorithm %q", env.Alg)
	}
	payload, err := decodeB64(env.Payload)
	if err != nil || len(payload) == 0 {
		return nil, errors.New("payload is not base64")
	}
	sig, err := decodeB64(env.Signature)
	if err != nil {
		return nil, errors.New("signature is not base64")
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(v.Key, payload, sig) {
		if v.KeyID != "" && env.KeyID != "" && env.KeyID != v.KeyID {
			return nil, fmt.Errorf("bad signature (signed with key %q, pinned key is %q — was the Nexus signing key rotated?)", env.KeyID, v.KeyID)
		}
		return nil, errors.New("bad signature")
	}

	var p Payload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("payload is not valid JSON: %v", err)
	}
	if p.V != PayloadVersion {
		return nil, fmt.Errorf("unsupported payload version %d", p.V)
	}
	if p.AgentID != v.AgentID {
		return nil, fmt.Errorf("plan was issued for agent %q, not this agent (%q)", p.AgentID, v.AgentID)
	}
	if p.TaskID == "" || p.TaskID != taskID {
		return nil, fmt.Errorf("signed task id %q does not match the delivered task %q", p.TaskID, taskID)
	}
	if p.ExpiresAt.IsZero() {
		return nil, errors.New("plan has no expiry")
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	skew := v.Skew
	if skew == 0 {
		skew = DefaultSkew
	}
	if t := now(); !t.Before(p.ExpiresAt.Add(skew)) {
		return nil, fmt.Errorf("plan expired at %s (now %s)", p.ExpiresAt.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
	}
	if len(p.Plan) == 0 || string(p.Plan) == "null" {
		return nil, errors.New("payload carries no plan")
	}
	if v.Executed != nil && v.Executed(p.TaskID) {
		return nil, fmt.Errorf("task %s was already executed (replay)", p.TaskID)
	}
	return &p, nil
}

func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// Pin is the public key the agent verifies plans against, and where it came
// from.
type Pin struct {
	Key    ed25519.PublicKey
	KeyID  string
	Source string // "env", "state" or "" (none pinned)
}

// Pinned reports whether a key is pinned.
func (p Pin) Pinned() bool { return p.Key != nil }

// ResolvePin applies the precedence LINEXUS_SIGNING_PUBKEY (envB64) > the key
// saved in the state file (saved) > none. The enrollment answer's key reaches
// the agent through the state file, where it is saved at enrollment.
func ResolvePin(envB64 string, saved *Key) (Pin, error) {
	if strings.TrimSpace(envB64) != "" {
		k, err := ParsePublicKey(envB64)
		if err != nil {
			return Pin{}, fmt.Errorf("LINEXUS_SIGNING_PUBKEY: %w", err)
		}
		p := Pin{Key: k, Source: "env"}
		if saved != nil {
			if sk, err := saved.Parse(); err == nil && sk.Equal(k) {
				p.KeyID = saved.KeyID
			}
		}
		return p, nil
	}
	if saved != nil && saved.PublicKey != "" {
		k, err := saved.Parse()
		if err != nil {
			return Pin{}, fmt.Errorf("signing key in the state file: %w", err)
		}
		return Pin{Key: k, KeyID: saved.KeyID, Source: "state"}, nil
	}
	return Pin{}, nil
}

// CheckEnrollmentKey decides which key to save at enrollment. got is the key
// Nexus advertised (nil when it sent none — an old Nexus). When the operator
// pinned a key through the environment (envB64), a different advertised key
// is refused: this Nexus is not the one the operator expects.
func CheckEnrollmentKey(envB64 string, got *Key) (*Key, error) {
	if got == nil || got.PublicKey == "" {
		return nil, nil
	}
	gk, err := got.Parse()
	if err != nil {
		return nil, fmt.Errorf("Nexus advertised an unusable signing key: %w", err)
	}
	if strings.TrimSpace(envB64) != "" {
		ek, err := ParsePublicKey(envB64)
		if err != nil {
			return nil, fmt.Errorf("LINEXUS_SIGNING_PUBKEY: %w", err)
		}
		if !ek.Equal(gk) {
			return nil, fmt.Errorf("Nexus signs with key %s (keyId %q) but LINEXUS_SIGNING_PUBKEY pins %s: "+
				"refusing to enroll with a Nexus whose signing key does not match the pinned one "+
				"(check NEXUS_URL, or update LINEXUS_SIGNING_PUBKEY if the key was rotated)",
				Fingerprint(gk), got.KeyID, Fingerprint(ek))
		}
	}
	k := Key{Alg: Alg, KeyID: got.KeyID, PublicKey: base64.StdEncoding.EncodeToString(gk)}
	return &k, nil
}
