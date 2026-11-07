// Package tlscert is the web listener's certificate with remote access: the one [tls] names, or a
// self-signed pair Meko makes in the data folder's certs/, the way Kode made its own (a Kode
// pair is copied there when Meko takes over Kode's port, so browsers that trusted it still do).
package tlscert

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// Lifetime and renewal of a self-signed pair: browsers cap a certificate at 825 days, and a pair is
// made again this long before it expires, so a browser never sees it lapse.
const (
	Lifetime    = 825 * 24 * time.Hour
	RenewBefore = 30 * 24 * time.Hour
)

// The self-signed pair's files in the certs folder (Kode's names).
const (
	CertName = "self-signed.crt"
	KeyName  = "self-signed.key"
)

// Pair is a loaded certificate.
type Pair struct {
	Cert        tls.Certificate
	Fingerprint string // SHA-256 of the leaf, colon-separated hex, as browsers show it
	SelfSigned  bool
	NotAfter    time.Time
}

// Load reads a certificate and its key (the [tls] files).
func Load(certFile, keyFile string) (*Pair, error) {
	c, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("can't load the certificate in [tls]: %w", err)
	}
	return pairOf(c, false)
}

// SelfSigned loads the self-signed pair in dir, making it first when it's missing, unreadable or
// expires within RenewBefore of now. Its names are this host's FQDN and host name, localhost,
// 127.0.0.1, ::1 and the addresses of its network interfaces.
func SelfSigned(dir string, now time.Time) (*Pair, error) {
	certFile, keyFile := filepath.Join(dir, CertName), filepath.Join(dir, KeyName)
	if c, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		if p, err := pairOf(c, true); err == nil && p.NotAfter.Sub(now) > RenewBefore {
			return p, nil
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := generate(now)
	if err != nil {
		return nil, err
	}
	// The key first: a cert without its key is made again at the next start anyway.
	if err = paths.WriteFile(keyFile, keyPEM); err != nil {
		return nil, err
	}
	if err = paths.WriteFile(certFile, certPEM); err != nil {
		return nil, err
	}
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return pairOf(c, true)
}

func pairOf(c tls.Certificate, selfSigned bool) (*Pair, error) {
	if len(c.Certificate) == 0 {
		return nil, errors.New("the certificate file has no certificate")
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return nil, err
	}
	c.Leaf = leaf
	return &Pair{Cert: c, Fingerprint: Fingerprint(c.Certificate[0]), SelfSigned: selfSigned, NotAfter: leaf.NotAfter}, nil
}

// Fingerprint is a DER certificate's SHA-256, as colon-separated upper-case hex.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	hex := make([]string, len(sum))
	for i, b := range sum {
		hex[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hex, ":")
}

func generate(now time.Time) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	names := unique([]string{FQDN(), host, "localhost"})
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if addrs, aerr := net.InterfaceAddrs(); aerr == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
				ips = append(ips, n.IP)
			}
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(Lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kder}), nil
}

// FQDN is this host's fully qualified name (hostname -f), else its host name.
func FQDN() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "hostname", "-f").Output(); err == nil {
		if f := strings.TrimSpace(string(out)); f != "" {
			return f
		}
	}
	h, _ := os.Hostname()
	return h
}

func unique(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
