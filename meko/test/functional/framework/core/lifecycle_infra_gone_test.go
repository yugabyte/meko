package core

import (
	"context"
	"testing"
)

type goneProv struct {
	EnvProvider
	where  string
	gone   bool
	shares bool
	calls  int
}

func (p *goneProv) SharesInfraAcrossTests() bool { return p.shares }

func (p *goneProv) SharedInfraGone(context.Context) (string, bool) {
	p.calls++
	return p.where, p.gone
}

func TestAttestSharedInfraGone(t *testing.T) {
	shared := Test{Env: EnvSpec{Sharing: Shared}}
	dedicated := Test{Env: EnvSpec{Shape: DedicatedStack}}

	for _, c := range []struct {
		name      string
		test      Test
		prov      EnvProvider
		want      bool
		wantAsked bool
	}{
		{"shared test, namespace gone", shared, &goneProv{where: "ns-1", gone: true, shares: true}, true, true},
		{"shared test, namespace present", shared, &goneProv{shares: true}, false, true},
		// The all-dedicated run: the shared namespace was never created, so its
		// absence must never reclassify a product failure.
		{"dedicated test is never attested", dedicated, &goneProv{where: "ns-1", gone: true, shares: true}, false, false},
		{"provider shares nothing", shared, &goneProv{where: "ns-1", gone: true}, false, false},
		{"provider cannot answer", shared, nil, false, false},
	} {
		reason, ok := attestSharedInfraGone(c.test, c.prov)
		if ok != c.want {
			t.Errorf("%s: ok = %v, want %v", c.name, ok, c.want)
		}
		if ok && reason == "" {
			t.Errorf("%s: attested with an empty reason", c.name)
		}
		if p, isProv := c.prov.(*goneProv); isProv && (p.calls > 0) != c.wantAsked {
			t.Errorf("%s: provider asked = %v, want %v", c.name, p.calls > 0, c.wantAsked)
		}
	}
}

// The reason carries the marker that keeps the retry from re-running a test
// against a namespace that cannot come back.
func TestSharedInfraGoneVerdictNeverRetries(t *testing.T) {
	reason, ok := attestSharedInfraGone(Test{Env: EnvSpec{Sharing: Shared}},
		&goneProv{where: "control-plane namespace ns-1", gone: true, shares: true})
	if !ok {
		t.Fatal("attestation did not fire")
	}
	summary := "endpoint ep-1 compute never became ready [" + reason + "]"
	if r, retryable := SchedulingReason(summary); retryable {
		t.Errorf("a deleted-namespace verdict is retryable as %q", r)
	}
}
