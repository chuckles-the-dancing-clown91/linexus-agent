package nexus

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// serverCA writes the httptest TLS server's certificate as a CA bundle.
func serverCA(t *testing.T, srv *httptest.Server) string {
	return writePEM(t, t.TempDir(), "ca.pem", "CERTIFICATE", srv.Certificate().Raw)
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"alg":"ed25519","keyId":"k1","publicKey":"` + base64.StdEncoding.EncodeToString(make([]byte, 32)) + `"}`))
	})
}

func TestTLSWithPrivateCA(t *testing.T) {
	srv := httptest.NewTLSServer(okHandler())
	defer srv.Close()
	ca := serverCA(t, srv)

	// Default trust (system roots) does not know the test CA.
	if _, err := New(srv.URL, "").SigningKey(); err == nil {
		t.Fatal("untrusted server certificate accepted")
	}
	for _, only := range []bool{false, true} {
		cfg, err := TLSOptions{CAFile: ca, CAOnly: only}.TLSConfig()
		if err != nil {
			t.Fatal(err)
		}
		k, err := NewTLS(srv.URL, "", cfg).SigningKey()
		if err != nil || k.KeyID != "k1" {
			t.Fatalf("caOnly=%v: key=%+v err=%v", only, k, err)
		}
	}
}

func TestTLSClientCertificate(t *testing.T) {
	dir := t.TempDir()
	// A private CA and a client certificate it issued.
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test agent CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	cliKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cliTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "agent-1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	cliDER, err := x509.CreateCertificate(rand.Reader, cliTmpl, caCert, &cliKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(cliKey)
	certFile := writePEM(t, dir, "client.pem", "CERTIFICATE", cliDER)
	keyFile := writePEM(t, dir, "client.key", "EC PRIVATE KEY", keyDER)

	var gotCN string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) > 0 {
			gotCN = r.TLS.PeerCertificates[0].Subject.CommonName
		}
		okHandler().ServeHTTP(w, r)
	}))
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	srv.StartTLS()
	defer srv.Close()
	ca := serverCA(t, srv)

	noCert, _ := TLSOptions{CAFile: ca, CAOnly: true}.TLSConfig()
	if _, err := NewTLS(srv.URL, "", noCert).SigningKey(); err == nil {
		t.Fatal("server requiring a client certificate accepted none")
	}
	withCert, err := TLSOptions{CAFile: ca, CAOnly: true, ClientCert: certFile, ClientKey: keyFile}.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTLS(srv.URL, "", withCert).SigningKey(); err != nil {
		t.Fatalf("mTLS: %v", err)
	}
	if gotCN != "agent-1" {
		t.Errorf("server saw client CN %q", gotCN)
	}
}

func TestTLSOptionsErrors(t *testing.T) {
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	for name, tc := range map[string]struct {
		o    TLSOptions
		want string
	}{
		"ca only without ca": {TLSOptions{CAOnly: true}, "LINEXUS_CA_FILE is not"},
		"missing ca file":    {TLSOptions{CAFile: filepath.Join(dir, "nope.pem")}, "LINEXUS_CA_FILE"},
		"ca without pem":     {TLSOptions{CAFile: junk}, "holds no PEM certificate"},
		"cert without key":   {TLSOptions{ClientCert: junk}, "must be set together"},
		"bad key pair":       {TLSOptions{ClientCert: junk, ClientKey: junk}, "LINEXUS_CLIENT_CERT"},
	} {
		if _, err := tc.o.TLSConfig(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if cfg, err := (TLSOptions{}).TLSConfig(); cfg != nil || err != nil {
		t.Errorf("empty options: %v %v", cfg, err)
	}
}

func TestCheckURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://nexus.example.com":   true,
		"https://10.0.0.5:5150":       true,
		"http://127.0.0.1:5150":       true,
		"http://localhost:5150":       true,
		"http://[::1]:5150":           true,
		"http://nexus.example.com":    false,
		"http://10.0.0.5:5150":        false,
		"http://localhost.evil.com":   false,
		"ftp://nexus.example.com":     false,
		"nexus.example.com:5150/path": false,
	} {
		if err := CheckURL(raw, false); (err == nil) != ok {
			t.Errorf("CheckURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
	if err := CheckURL("http://nexus:5150", true); err != nil {
		t.Errorf("LINEXUS_ALLOW_INSECURE should permit http: %v", err)
	}
	if err := CheckURL("ftp://nexus", true); err == nil {
		t.Error("LINEXUS_ALLOW_INSECURE must not permit other schemes")
	}
}
