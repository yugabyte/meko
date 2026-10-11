package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yugabyte/meko/meko/test/functional/framework/core"
	"github.com/yugabyte/meko/meko/test/functional/framework/env/fake"
)

// preparingProvider is a fake provider with the optional Prepare/Close seams.
type preparingProvider struct {
	*fake.Provider
	err     error
	prepare int
	closed  int
}

func (p *preparingProvider) Prepare(context.Context) error { p.prepare++; return p.err }
func (p *preparingProvider) Close() error                  { p.closed++; return nil }

func zeroBackoff(t *testing.T) *core.Config {
	t.Helper()
	cfg, err := core.LoadConfig(nil, nil, []string{"sched.env_retry_backoff=0s"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

// An unplaceable control plane is torn down and the provider REBUILT.
func TestPrepareProviderRetriesSchedulingFailure(t *testing.T) {
	first := &preparingProvider{Provider: &fake.Provider{},
		err: errors.New("helm install exited 1: Error: INSTALLATION FAILED: context deadline exceeded")}
	second := &preparingProvider{Provider: &fake.Provider{}}
	rebuilt := 0

	got, err := prepareProvider(context.Background(), first, zeroBackoff(t), func() (core.EnvProvider, error) {
		rebuilt++
		return second, nil
	})
	if err != nil {
		t.Fatalf("prepare = %v, want success on the retry", err)
	}
	if rebuilt != 1 || got != core.EnvProvider(second) {
		t.Errorf("rebuilt %d times, returned %T, want one rebuild and the new provider", rebuilt, got)
	}
	if first.closed != 1 {
		t.Errorf("half-built provider closed %d times, want 1", first.closed)
	}
	if second.prepare != 1 {
		t.Errorf("rebuilt provider prepared %d times, want 1", second.prepare)
	}
}

// A bad image tag or a credential problem fails identically every attempt.
func TestPrepareProviderDoesNotRetryHardFailures(t *testing.T) {
	p := &preparingProvider{Provider: &fake.Provider{},
		err: errors.New("deployment never became Available: ImagePullBackOff meko-api-server:custom-nope")}
	rebuilt := 0
	_, err := prepareProvider(context.Background(), p, zeroBackoff(t), func() (core.EnvProvider, error) {
		rebuilt++
		return p, nil
	})
	if err == nil {
		t.Fatal("prepare must fail")
	}
	if p.prepare != 1 || rebuilt != 0 {
		t.Errorf("prepared %d times with %d rebuilds, want 1 and 0", p.prepare, rebuilt)
	}
}

// TestPrepareProviderRetriesOff: sched.env_retries=0 turns the retry off.
func TestPrepareProviderRetriesOff(t *testing.T) {
	cfg, err := core.LoadConfig(nil, nil, []string{"sched.env_retry_backoff=0s", "sched.env_retries=int:0"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	p := &preparingProvider{Provider: &fake.Provider{}, err: errors.New("pod unschedulable")}
	if _, err := prepareProvider(context.Background(), p, cfg, func() (core.EnvProvider, error) {
		t.Fatal("must not rebuild when retries are off")
		return nil, nil
	}); err == nil {
		t.Fatal("prepare must fail")
	}
	if p.prepare != 1 {
		t.Errorf("prepared %d times, want 1", p.prepare)
	}
}

// Providers with no Prepare seam (fake) are untouched.
func TestPrepareProviderPassesThroughWithoutPrepare(t *testing.T) {
	p := &fake.Provider{}
	got, err := prepareProvider(context.Background(), p, zeroBackoff(t), func() (core.EnvProvider, error) {
		t.Fatal("must not rebuild a provider with no Prepare")
		return nil, nil
	})
	if err != nil || got != core.EnvProvider(p) {
		t.Errorf("prepare = (%T, %v), want the same provider and no error", got, err)
	}
}

func stackTests(shapes ...core.Shape) []core.Test {
	var out []core.Test
	for i, s := range shapes {
		out = append(out, core.Test{Name: fmt.Sprintf("t%d", i), Env: core.EnvSpec{Sharing: core.Exclusive, Shape: s}})
	}
	return out
}

func TestPrepareSharedStackFallsBackForOffStackTests(t *testing.T) {
	failed := &preparingProvider{Provider: &fake.Provider{}, err: errors.New("Dirty database version 1. Fix and force version.")}
	fresh := &preparingProvider{Provider: &fake.Provider{}}
	got, reason, err := prepareSharedStack(context.Background(), stackTests(core.SingleStack, core.ClusterOnly), failed, zeroBackoff(t), true,
		func() (core.EnvProvider, error) { return fresh, nil })
	if err != nil {
		t.Fatalf("prepare = %v, want the run to go on", err)
	}
	if got != core.EnvProvider(fresh) || fresh.prepare != 0 {
		t.Errorf("got %T prepared %d times, want the fresh provider, never prepared", got, fresh.prepare)
	}
	if !strings.Contains(reason, "Dirty database version 1") {
		t.Errorf("reason = %q, want the prepare error", reason)
	}
}

func TestPrepareSharedStackFailsWhenEveryTestNeedsIt(t *testing.T) {
	for name, tc := range map[string]struct {
		tests    []core.Test
		offStack bool
	}{
		"all on the stack": {stackTests(core.SingleStack, core.SingleStack), true},
		"setup only":       {stackTests(core.SingleStack, core.ClusterOnly), false},
	} {
		p := &preparingProvider{Provider: &fake.Provider{}, err: errors.New("ImagePullBackOff")}
		_, reason, err := prepareSharedStack(context.Background(), tc.tests, p, zeroBackoff(t), tc.offStack,
			func() (core.EnvProvider, error) { t.Fatalf("%s: must not rebuild", name); return nil, nil })
		if err == nil || reason != "" {
			t.Errorf("%s: (%q, %v), want the prepare error", name, reason, err)
		}
	}
}

func TestPrepareSharedStackSkippedWithNoTestOnIt(t *testing.T) {
	p := &preparingProvider{Provider: &fake.Provider{}}
	if _, _, err := prepareSharedStack(context.Background(), stackTests(core.ClusterOnly, core.DedicatedStack, core.Remote), p, zeroBackoff(t), true,
		func() (core.EnvProvider, error) { return nil, nil }); err != nil || p.prepare != 0 {
		t.Errorf("prepared %d times (%v), want no shared stack", p.prepare, err)
	}
}

type rebuildingPrepProvider struct {
	preparingProvider
	rebuilds int
}

func (p *rebuildingPrepProvider) RebuildSharedInfra(ctx context.Context) error {
	p.rebuilds++
	p.err = nil
	return p.Prepare(ctx)
}

// A stack that came up broken gets one fresh stack on the same provider, so a
// run's YBA is not stood up again.
func TestPrepareProviderRebuildsTheStackOnceForAProductFailure(t *testing.T) {
	p := &rebuildingPrepProvider{preparingProvider: preparingProvider{Provider: &fake.Provider{},
		err: errors.New("api-server migrations wedged on a dirty version")}}
	got, err := prepareProvider(context.Background(), p, zeroBackoff(t), func() (core.EnvProvider, error) {
		t.Fatal("the provider must not be rebuilt")
		return nil, nil
	})
	if err != nil || got != core.EnvProvider(p) {
		t.Fatalf("prepare = %v, %T, want success on the same provider", err, got)
	}
	if p.rebuilds != 1 || p.closed != 0 {
		t.Errorf("stack rebuilds = %d, closes = %d, want 1 and 0", p.rebuilds, p.closed)
	}
}

func TestPrepareProviderDoesNotRebuildForABadImage(t *testing.T) {
	p := &rebuildingPrepProvider{preparingProvider: preparingProvider{Provider: &fake.Provider{},
		err: errors.New("deployment never became Available: ImagePullBackOff meko-api-server:custom-nope")}}
	if _, err := prepareProvider(context.Background(), p, zeroBackoff(t), func() (core.EnvProvider, error) { return p, nil }); err == nil {
		t.Fatal("prepare must fail")
	}
	if p.rebuilds != 0 {
		t.Errorf("stack rebuilds = %d, want 0", p.rebuilds)
	}
}
