package features

import (
	"strings"
	"testing"
)

const (
	rootA = "-----BEGIN CERTIFICATE-----\nAAAA\nBBBB\n-----END CERTIFICATE-----\n"
	rootB = "-----BEGIN CERTIFICATE-----\nCCCC\nDDDD\n-----END CERTIFICATE-----\n"
)

func TestAppendRootPEMAddsOnce(t *testing.T) {
	bundle := appendRootPEM(rootA, rootB)
	if !containsPEM(bundle, rootA) || !containsPEM(bundle, rootB) {
		t.Fatalf("bundle lost a root:\n%s", bundle)
	}
	if again := appendRootPEM(bundle, rootB); again != bundle {
		t.Fatalf("appending a root already present must not change the bundle:\n%s", again)
	}
	if strings.Count(bundle, "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("want two certificates, got:\n%s", bundle)
	}
}

func TestContainsPEMIgnoresWhitespace(t *testing.T) {
	rewrapped := "-----BEGIN CERTIFICATE-----\nAAAABBBB\n-----END CERTIFICATE-----"
	if !containsPEM(rootA+rootB, rewrapped) {
		t.Fatal("the same certificate wrapped differently must be found")
	}
	if containsPEM(rootA, rootB) {
		t.Fatal("a different certificate must not be found")
	}
	if containsPEM(rootA, "") {
		t.Fatal("an empty certificate is never contained")
	}
}

func TestSamePEM(t *testing.T) {
	if !samePEM(rootA, "-----BEGIN CERTIFICATE-----\nAAAA BBBB\n-----END CERTIFICATE-----\n") {
		t.Fatal("same body, different wrapping: want equal")
	}
	if samePEM(rootA, rootB) {
		t.Fatal("different bodies: want unequal")
	}
}

func TestTLSRotationGateSkipsUnlessOwnedIssuer(t *testing.T) {
	cfg := tlsRotationConfig{Enabled: false, IssuerKind: "ClusterIssuer"}
	if reason, ok := cfg.gate(nil, nil); ok || !strings.Contains(reason, "tls_rotation.enabled") {
		t.Fatalf("disabled rehearsal must skip naming the knob, got ok=%v reason=%q", ok, reason)
	}
}

// The rehearsal repoints a cluster-scoped ClusterIssuer, and the scheduler
// cannot serialise that against another run's stack. Pointed at the shared
// default it must refuse rather than rotate trust out from under one.
func TestTLSRotationRefusesTheSharedIssuer(t *testing.T) {
	cfg := tlsRotationConfig{Enabled: true, IssuerKind: "ClusterIssuer", IssuerName: defaultRotationIssuerName}
	reason, ok := cfg.gate(nil, nil)
	if ok {
		t.Fatal("the rehearsal accepted the cluster-wide default issuer")
	}
	if !strings.Contains(reason, "issuer_name") {
		t.Fatalf("skip reason should name the knob to change, got %q", reason)
	}
}
