package handlers

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// awsSNSCertHostPattern gates which hosts we'll fetch the signing
// certificate from. AWS SNS docs guarantee cert URLs look like
// https://sns.<region>.amazonaws.com/... (or .amazonaws.com.cn/... for
// China regions). Anything else is a red flag: an attacker crafting a
// fake SNS payload could point us at a cert they control, letting
// them "sign" arbitrary notifications. Restricting the URL host
// prevents that class of attack.
var awsSNSCertHostPattern = regexp.MustCompile(`^sns\.[a-z0-9-]+\.amazonaws\.com(\.cn)?$`)

// snsCertCache stores fetched X.509 certs so a burst of SNS
// notifications doesn't re-fetch the same cert repeatedly. Certs
// rotate rarely; a process-lifetime cache is fine.
var snsCertCache sync.Map // certURL string -> *x509.Certificate

// snsMessage is the envelope AWS SNS posts to our HTTPS endpoint. All
// fields are exposed so the caller can inspect Type before dispatch.
type snsMessage struct {
	Type             string `json:"Type"`
	MessageID        string `json:"MessageId"`
	Token            string `json:"Token,omitempty"` // present on Confirmation types
	TopicARN         string `json:"TopicArn"`
	Subject          string `json:"Subject,omitempty"`
	Message          string `json:"Message"`
	Timestamp        string `json:"Timestamp"`
	SignatureVersion string `json:"SignatureVersion"`
	Signature        string `json:"Signature"`
	SigningCertURL   string `json:"SigningCertURL"`
	SubscribeURL     string `json:"SubscribeURL,omitempty"`
	UnsubscribeURL   string `json:"UnsubscribeURL,omitempty"`
}

// verifySNSSignature validates that the SNS envelope was actually
// posted by AWS. Steps:
//  1. Reject unsupported SignatureVersion / missing signature.
//  2. Reject SigningCertURL that isn't from an AWS SNS domain.
//  3. Fetch (or read from cache) the X.509 signing cert.
//  4. Extract the RSA public key.
//  5. Rebuild the canonical signing string per SNS spec.
//  6. Verify PKCS#1 v1.5 signature over SHA1 (v1) or SHA256 (v2).
//
// Any failure here means the caller MUST NOT trust the payload.
func verifySNSSignature(msg *snsMessage) error {
	if msg.SignatureVersion != "1" && msg.SignatureVersion != "2" {
		return fmt.Errorf("unsupported signature version %q", msg.SignatureVersion)
	}
	if msg.Signature == "" || msg.SigningCertURL == "" {
		return errors.New("missing signature or signing cert URL")
	}

	certURL, err := url.Parse(msg.SigningCertURL)
	if err != nil {
		return fmt.Errorf("parse cert URL: %w", err)
	}
	if certURL.Scheme != "https" {
		return errors.New("cert URL must be HTTPS")
	}
	if !awsSNSCertHostPattern.MatchString(certURL.Host) {
		return fmt.Errorf("cert URL host %q is not a recognized AWS SNS domain", certURL.Host)
	}

	cert, err := fetchSNSCert(msg.SigningCertURL)
	if err != nil {
		return fmt.Errorf("fetch cert: %w", err)
	}
	rsaPub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return errors.New("SNS signing cert does not carry an RSA public key")
	}

	canonical := buildSNSCanonicalString(msg)
	signature, err := base64.StdEncoding.DecodeString(msg.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	var hashFunc crypto.Hash
	var hashed []byte
	if msg.SignatureVersion == "1" {
		sum := sha1.Sum([]byte(canonical))
		hashed = sum[:]
		hashFunc = crypto.SHA1
	} else {
		sum := sha256.Sum256([]byte(canonical))
		hashed = sum[:]
		hashFunc = crypto.SHA256
	}
	if err := rsa.VerifyPKCS1v15(rsaPub, hashFunc, hashed, signature); err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}
	return nil
}

// buildSNSCanonicalString assembles the newline-delimited key/value
// blob AWS SNS signs. Field order is FIXED by AWS (not alphabetical);
// deviating produces a different hash and the signature won't verify.
// See docs: docs.aws.amazon.com/sns/latest/dg/sns-verify-signature-of-message.html
func buildSNSCanonicalString(msg *snsMessage) string {
	var b strings.Builder
	switch msg.Type {
	case "Notification":
		b.WriteString("Message\n")
		b.WriteString(msg.Message)
		b.WriteString("\n")
		b.WriteString("MessageId\n")
		b.WriteString(msg.MessageID)
		b.WriteString("\n")
		if msg.Subject != "" {
			b.WriteString("Subject\n")
			b.WriteString(msg.Subject)
			b.WriteString("\n")
		}
		b.WriteString("Timestamp\n")
		b.WriteString(msg.Timestamp)
		b.WriteString("\n")
		b.WriteString("TopicArn\n")
		b.WriteString(msg.TopicARN)
		b.WriteString("\n")
		b.WriteString("Type\n")
		b.WriteString(msg.Type)
		b.WriteString("\n")
	case "SubscriptionConfirmation", "UnsubscribeConfirmation":
		b.WriteString("Message\n")
		b.WriteString(msg.Message)
		b.WriteString("\n")
		b.WriteString("MessageId\n")
		b.WriteString(msg.MessageID)
		b.WriteString("\n")
		b.WriteString("SubscribeURL\n")
		b.WriteString(msg.SubscribeURL)
		b.WriteString("\n")
		b.WriteString("Timestamp\n")
		b.WriteString(msg.Timestamp)
		b.WriteString("\n")
		b.WriteString("Token\n")
		b.WriteString(msg.Token)
		b.WriteString("\n")
		b.WriteString("TopicArn\n")
		b.WriteString(msg.TopicARN)
		b.WriteString("\n")
		b.WriteString("Type\n")
		b.WriteString(msg.Type)
		b.WriteString("\n")
	}
	return b.String()
}

// fetchSNSCert downloads AWS's SNS signing cert (a PEM-encoded X.509
// leaf), parses it, and caches by URL. AWS rotates certs infrequently
// and each has a stable URL, so a process-lifetime cache is fine.
func fetchSNSCert(certURL string) (*x509.Certificate, error) {
	if cached, ok := snsCertCache.Load(certURL); ok {
		return cached.(*x509.Certificate), nil
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(certURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cert fetch returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024)) // certs are ~2KB; cap at 64KB
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("no PEM block in signing cert response")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	snsCertCache.Store(certURL, cert)
	return cert, nil
}
