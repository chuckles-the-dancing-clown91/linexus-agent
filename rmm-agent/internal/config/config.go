// Package config loads the RMM agent's runtime configuration from the
// environment. Every value has a sensible default so the agent runs out of the
// box against a local Nexus.
package config

import (
	"os"
	"strconv"
	"time"
)

type Config struct {
	// NexusURL is the base URL of the Nexus gateway (the only service the
	// agent talks to).
	NexusURL string
	// Token is the legacy shared credential (AGENT_TOKEN): a Nexus system key
	// presented as `Authorization: Bearer <token>`. It enrolls when no
	// EnrollmentToken is set, and is the bearer for agents whose state file
	// holds no per-agent credential (enrolled before Nexus issued them).
	Token string
	// EnrollmentToken is a one-time `nxe_…` token minted by Nexus (normally
	// carried by the install command). It is sent in the enroll body with no
	// bearer; Nexus answers with a per-agent `nxa_…` credential that the agent
	// persists and uses for every later call. Ignored once enrolled.
	EnrollmentToken string
	// Hostgroup is an optional enrollment hint (e.g. "web-prod").
	Hostgroup string
	// StateFile persists the assigned agent id across restarts.
	StateFile string
	// PollInterval is how often the agent asks Nexus for new tasks.
	PollInterval time.Duration
	// HeartbeatInterval is how often the agent reports liveness.
	HeartbeatInterval time.Duration
	// FactsInterval is how often the agent ships a full facts report
	// (inventory, interfaces, listening sockets, services, packages, DNS).
	FactsInterval time.Duration
	// AllowDestructive gates system.reboot / power actions. Off by default so
	// the agent never takes a box down unless explicitly permitted.
	AllowDestructive bool
	// Once runs a single enroll→report→poll→execute cycle then exits. Useful
	// for testing and one-shot invocations.
	Once bool

	// SigningPubKey pins Nexus's plan-signing Ed25519 public key (base64,
	// LINEXUS_SIGNING_PUBKEY). It wins over the key saved at enrollment.
	SigningPubKey string
	// AllowUnsigned lets the agent run unsigned plans while no signing key is
	// pinned — only for a Nexus too old to sign (LINEXUS_ALLOW_UNSIGNED=1).
	AllowUnsigned bool
	// CAFile is a PEM bundle trusted for Nexus's certificate, added to the
	// system roots, or used alone with CAOnly (LINEXUS_CA_FILE,
	// LINEXUS_CA_ONLY).
	CAFile string
	CAOnly bool
	// ClientCert/ClientKey: an mTLS client certificate presented to Nexus
	// (LINEXUS_CLIENT_CERT, LINEXUS_CLIENT_KEY).
	ClientCert string
	ClientKey  string
	// AllowInsecure permits a plain-http NEXUS_URL to a non-loopback host
	// (LINEXUS_ALLOW_INSECURE=1).
	AllowInsecure bool
}

func Load() Config {
	return Config{
		NexusURL:          envStr("NEXUS_URL", "http://127.0.0.1:5150"),
		Token:             os.Getenv("AGENT_TOKEN"),
		EnrollmentToken:   os.Getenv("ENROLLMENT_TOKEN"),
		Hostgroup:         os.Getenv("AGENT_HOSTGROUP"),
		StateFile:         envStr("AGENT_STATE_FILE", "linexus-agent-state.json"),
		PollInterval:      envDur("AGENT_POLL_INTERVAL", 10*time.Second),
		HeartbeatInterval: envDur("AGENT_HEARTBEAT_INTERVAL", 30*time.Second),
		FactsInterval:     envDur("AGENT_FACTS_INTERVAL", 5*time.Minute),
		AllowDestructive:  envBool("AGENT_ALLOW_DESTRUCTIVE", false),
		Once:              envBool("AGENT_ONCE", false),
		SigningPubKey:     os.Getenv("LINEXUS_SIGNING_PUBKEY"),
		AllowUnsigned:     envBool("LINEXUS_ALLOW_UNSIGNED", false),
		CAFile:            os.Getenv("LINEXUS_CA_FILE"),
		CAOnly:            envBool("LINEXUS_CA_ONLY", false),
		ClientCert:        os.Getenv("LINEXUS_CLIENT_CERT"),
		ClientKey:         os.Getenv("LINEXUS_CLIENT_KEY"),
		AllowInsecure:     envBool("LINEXUS_ALLOW_INSECURE", false),
	}
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
