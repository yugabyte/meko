package db

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// writeSelfSigned writes a self-signed certificate and key with the given
// serial to dir and returns their paths.
func writeSelfSigned(t *testing.T, dir string, serial int64) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "yugabyte"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "tls.crt")
	keyPath = filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func serialOf(t *testing.T, c *tls.Certificate) int64 {
	t.Helper()
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.Int64()
}

// TestClientCertReloadFollowsTheFiles asserts the pool's TLS config presents
// whatever certificate is on disk at handshake time, so a renewal is picked up
// without reconnecting the pool, on every host of a multi-host DSN.
func TestClientCertReloadFollowsTheFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, 1)
	dsn := "postgres://yugabyte@h1:5433,h2:5433/yb_storage?sslmode=verify-full&sslrootcert=" + certPath +
		"&sslcert=" + certPath + "&sslkey=" + keyPath
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := installClientCertReload(cfg, dsn); err != nil {
		t.Fatalf("install: %v", err)
	}
	tlsConfigs := []*tls.Config{cfg.ConnConfig.TLSConfig}
	for _, fb := range cfg.ConnConfig.Fallbacks {
		tlsConfigs = append(tlsConfigs, fb.TLSConfig)
	}
	if len(tlsConfigs) < 2 {
		t.Fatalf("expected a TLS config per host, got %d", len(tlsConfigs))
	}
	for _, tc := range tlsConfigs {
		if tc.GetClientCertificate == nil || len(tc.Certificates) != 0 {
			t.Fatalf("static certificate still installed: %d certs, reload=%v", len(tc.Certificates), tc.GetClientCertificate != nil)
		}
		c, err := tc.GetClientCertificate(&tls.CertificateRequestInfo{})
		if err != nil {
			t.Fatal(err)
		}
		if got := serialOf(t, c); got != 1 {
			t.Errorf("serial = %d, want 1", got)
		}
	}

	// Renewal: the files change under the running pool.
	writeSelfSigned(t, dir, 2)
	c, err := cfg.ConnConfig.TLSConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if got := serialOf(t, c); got != 2 {
		t.Errorf("serial after renewal = %d, want 2", got)
	}
}

// TestClientCertReloadNoop asserts a DSN without a client certificate, or
// without TLS, is untouched, and a broken pair fails at install naming the file.
func TestClientCertReloadNoop(t *testing.T) {
	dsn := "postgres://yugabyte@h:5433/yb_storage?sslmode=disable"
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := installClientCertReload(cfg, dsn); err != nil {
		t.Fatalf("plaintext: %v", err)
	}

	dir := t.TempDir()
	certPath, _ := writeSelfSigned(t, dir, 3)
	dsn = "postgres://yugabyte@h:5433/yb_storage?sslmode=verify-full&sslrootcert=" + certPath
	cfg, err = pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := installClientCertReload(cfg, dsn); err != nil {
		t.Fatalf("server-auth only: %v", err)
	}
	if cfg.ConnConfig.TLSConfig.GetClientCertificate != nil {
		t.Error("reload installed without a client certificate")
	}

	missing := filepath.Join(dir, "absent.key")
	dsn = dsn + "&sslcert=" + certPath + "&sslkey=" + missing
	if _, err := pgxpool.ParseConfig(dsn); err == nil {
		// pgx itself may or may not reject the missing key at parse time; either
		// way the reload installer must name the file.
		cfg, _ = pgxpool.ParseConfig(dsn)
		if cfg != nil {
			if err := installClientCertReload(cfg, dsn); err == nil {
				t.Error("expected an error for a missing key file")
			}
		}
	}
}

func TestDSNParam(t *testing.T) {
	if got := dsnParam("postgres://u@h/db?sslmode=verify-full&sslcert=/a/tls.crt", "sslcert"); got != "/a/tls.crt" {
		t.Errorf("url form: %q", got)
	}
	if got := dsnParam("host=h user=u sslcert='/a/tls.crt' sslkey=/a/tls.key", "sslkey"); got != "/a/tls.key" {
		t.Errorf("keyword form: %q", got)
	}
	if got := dsnParam("host=h user=u", "sslcert"); got != "" {
		t.Errorf("absent: %q", got)
	}
}

// The PG* fallback exists so a certificate configured pgx's other way is still
// reloaded; the DSN wins when both name one, as it does in pgx.
func TestDSNParamFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("PGSSLCERT", "/env/tls.crt")
	t.Setenv("PGSSLKEY", "/env/tls.key")

	if got := dsnParam("postgres://u@h/db?sslmode=verify-full", "sslcert"); got != "/env/tls.crt" {
		t.Errorf("url form with no sslcert: %q, want the environment's", got)
	}
	if got := dsnParam("host=h user=u", "sslkey"); got != "/env/tls.key" {
		t.Errorf("keyword form with no sslkey: %q, want the environment's", got)
	}
	if got := dsnParam("postgres://u@h/db?sslcert=/dsn/tls.crt", "sslcert"); got != "/dsn/tls.crt" {
		t.Errorf("the DSN must win over the environment, got %q", got)
	}
}

// A pool whose certificate came from PGSSLCERT/PGSSLKEY reloads like any other.
func TestClientCertReloadFromTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSigned(t, dir, 7)
	t.Setenv("PGSSLCERT", certPath)
	t.Setenv("PGSSLKEY", keyPath)

	dsn := "postgres://yugabyte@h:5433/yb_storage?sslmode=verify-full&sslrootcert=" + certPath
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if err := installClientCertReload(cfg, dsn); err != nil {
		t.Fatalf("install: %v", err)
	}
	tc := cfg.ConnConfig.TLSConfig
	if tc.GetClientCertificate == nil {
		t.Fatal("no reload installed for an environment-configured certificate")
	}
	writeSelfSigned(t, dir, 8)
	c, err := tc.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if got := serialOf(t, c); got != 8 {
		t.Errorf("serial after renewal = %d, want 8", got)
	}
}
