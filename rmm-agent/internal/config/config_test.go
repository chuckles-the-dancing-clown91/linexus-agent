package config

import (
	"testing"
	"time"
)

func TestLoadDefaultsAndOverrides(t *testing.T) {
	t.Setenv("ENROLLMENT_TOKEN", "")
	t.Setenv("AGENT_FACTS_INTERVAL", "")
	c := Load()
	if c.FactsInterval != 5*time.Minute || c.EnrollmentToken != "" {
		t.Errorf("defaults = %+v", c)
	}
	t.Setenv("ENROLLMENT_TOKEN", "nxe_abc")
	t.Setenv("AGENT_FACTS_INTERVAL", "90s")
	c = Load()
	if c.FactsInterval != 90*time.Second || c.EnrollmentToken != "nxe_abc" {
		t.Errorf("overrides = %+v", c)
	}
	t.Setenv("AGENT_FACTS_INTERVAL", "-1s")
	if Load().FactsInterval != 5*time.Minute {
		t.Error("a non-positive interval must fall back to the default (a ticker panics on <= 0)")
	}
}
