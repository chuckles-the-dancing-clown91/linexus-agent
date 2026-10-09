package nexus

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// TLSOptions configures how the agent verifies Nexus and, optionally,
// presents a client certificate (mTLS).
type TLSOptions struct {
	// CAFile is a PEM bundle trusted for Nexus's certificate, added to the
	// system roots (LINEXUS_CA_FILE).
	CAFile string
	// CAOnly trusts CAFile alone instead of adding it to the system roots
	// (LINEXUS_CA_ONLY=1).
	CAOnly bool
	// ClientCert and ClientKey are a PEM client certificate and its key,
	// presented to Nexus (LINEXUS_CLIENT_CERT, LINEXUS_CLIENT_KEY).
	ClientCert string
	ClientKey  string
}

// TLSConfig builds the client TLS configuration. It returns nil (the Go
// defaults: system roots, no client certificate) when nothing is configured.
func (o TLSOptions) TLSConfig() (*tls.Config, error) {
	if o.CAFile == "" && o.ClientCert == "" && o.ClientKey == "" {
		if o.CAOnly {
			return nil, errors.New("LINEXUS_CA_ONLY is set but LINEXUS_CA_FILE is not")
		}
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("LINEXUS_CA_FILE: %w", err)
		}
		var pool *x509.CertPool
		if !o.CAOnly {
			if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
				pool = sys
			}
		}
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("LINEXUS_CA_FILE %s holds no PEM certificate", o.CAFile)
		}
		cfg.RootCAs = pool
	} else if o.CAOnly {
		return nil, errors.New("LINEXUS_CA_ONLY is set but LINEXUS_CA_FILE is not")
	}
	switch {
	case o.ClientCert != "" && o.ClientKey != "":
		pair, err := tls.LoadX509KeyPair(o.ClientCert, o.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("LINEXUS_CLIENT_CERT/LINEXUS_CLIENT_KEY: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	case o.ClientCert != "" || o.ClientKey != "":
		return nil, errors.New("LINEXUS_CLIENT_CERT and LINEXUS_CLIENT_KEY must be set together")
	}
	return cfg, nil
}

// CheckURL refuses a plain-http Nexus URL unless allowInsecure is set or the
// host is loopback (a Nexus on the same machine). https is always fine.
func CheckURL(raw string, allowInsecure bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("NEXUS_URL %q: %w", raw, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return nil
	case "http":
		if allowInsecure || isLoopback(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("NEXUS_URL %q is plain http to a non-loopback host: the agent credential and every plan "+
			"would cross the network unencrypted. Use https (LINEXUS_CA_FILE for a private CA), or set "+
			"LINEXUS_ALLOW_INSECURE=1 for a lab", raw)
	default:
		return fmt.Errorf("NEXUS_URL %q: scheme must be https (or http to loopback)", raw)
	}
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
