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

func TestLoadTrustSettings(t *testing.T) {
	for k, v := range map[string]string{
		"LINEXUS_SIGNING_PUBKEY": "cHVi", "LINEXUS_ALLOW_UNSIGNED": "1", "LINEXUS_CA_FILE": "/etc/linexus/nexus-ca.pem",
		"LINEXUS_CA_ONLY": "true", "LINEXUS_CLIENT_CERT": "/c.pem", "LINEXUS_CLIENT_KEY": "/c.key", "LINEXUS_ALLOW_INSECURE": "1",
	} {
		t.Setenv(k, v)
	}
	c := Load()
	if c.SigningPubKey != "cHVi" || !c.AllowUnsigned || c.CAFile != "/etc/linexus/nexus-ca.pem" || !c.CAOnly ||
		c.ClientCert != "/c.pem" || c.ClientKey != "/c.key" || !c.AllowInsecure {
		t.Errorf("trust settings = %+v", c)
	}
	t.Setenv("LINEXUS_ALLOW_UNSIGNED", "")
	t.Setenv("LINEXUS_ALLOW_INSECURE", "")
	if c := Load(); c.AllowUnsigned || c.AllowInsecure {
		t.Error("ALLOW_* must default to off")
	}
}
