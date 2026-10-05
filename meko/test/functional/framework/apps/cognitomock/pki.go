package cognitomock

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"time"
)

const certLifetime = 365 * 24 * time.Hour

// Material is everything one mock needs to be trusted: the key its tokens are
// signed with, and a CA with a serving certificate for the Cognito hostname.
type Material struct {
	Key         *rsa.PrivateKey
	CAPEM       []byte
	CertPEM     []byte
	KeyPEM      []byte
	TokenKeyPEM []byte
}

func NewMaterial(host string, now time.Time, r io.Reader) (Material, error) {
	if r == nil {
		r = rand.Reader
	}
	key, err := rsa.GenerateKey(r, 2048)
	if err != nil {
		return Material{}, fmt.Errorf("generate token key: %w", err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), r)
	if err != nil {
		return Material{}, fmt.Errorf("generate ca key: %w", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), r)
	if err != nil {
		return Material{}, fmt.Errorf("generate serving key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(r),
		Subject:               pkix.Name{CommonName: "mekoqa cognito mock ca"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(certLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(r, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return Material{}, fmt.Errorf("create ca: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return Material{}, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(r),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(certLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(r, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return Material{}, fmt.Errorf("create serving certificate: %w", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return Material{}, err
	}
	return Material{
		Key:         key,
		CAPEM:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		CertPEM:     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		KeyPEM:      pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
		TokenKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}),
	}, nil
}

func serial(r io.Reader) *big.Int {
	n, err := rand.Int(r, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
