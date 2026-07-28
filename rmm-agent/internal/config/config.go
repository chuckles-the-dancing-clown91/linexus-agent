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
	// Token is presented to Nexus as `Authorization: Bearer <token>`.
	Token string
	// Hostgroup is an optional enrollment hint (e.g. "web-prod").
	Hostgroup string
	// StateFile persists the assigned agent id across restarts.
	StateFile string
	// PollInterval is how often the agent asks Nexus for new tasks.
	PollInterval time.Duration
	// HeartbeatInterval is how often the agent reports liveness.
	HeartbeatInterval time.Duration
	// AllowDestructive gates system.reboot / power actions. Off by default so
	// the agent never takes a box down unless explicitly permitted.
	AllowDestructive bool
	// Once runs a single enroll→report→poll→execute cycle then exits. Useful
	// for testing and one-shot invocations.
	Once bool
}

func Load() Config {
	return Config{
		NexusURL:          envStr("NEXUS_URL", "http://127.0.0.1:5150"),
		Token:             os.Getenv("AGENT_TOKEN"),
		Hostgroup:         os.Getenv("AGENT_HOSTGROUP"),
		StateFile:         envStr("AGENT_STATE_FILE", "linexus-agent-state.json"),
		PollInterval:      envDur("AGENT_POLL_INTERVAL", 10*time.Second),
		HeartbeatInterval: envDur("AGENT_HEARTBEAT_INTERVAL", 30*time.Second),
		AllowDestructive:  envBool("AGENT_ALLOW_DESTRUCTIVE", false),
		Once:              envBool("AGENT_ONCE", false),
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
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
