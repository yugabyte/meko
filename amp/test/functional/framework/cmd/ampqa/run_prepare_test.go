package main

import (
	"context"
	"errors"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/env/fake"
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
		t.Errorf("rebuilt %d times, returned %T — want one rebuild and the new provider", rebuilt, got)
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
		err: errors.New("controller never became Available: ImagePullBackOff amp-controller:custom-nope")}
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

// Providers with no Prepare seam (universe, fake) are untouched.
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
