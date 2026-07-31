// Package fake is an in-memory environment provider used by unit tests and by
// --dry-run. It is configurable to succeed, fail provisioning, delay, record
// teardown calls, and report live-environment concurrency for scheduler tests.
package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
	"github.com/yugabyte/amp/test/functional/framework/exec"
)

// Provider is a fake EnvProvider.
type Provider struct {
	FailProvision  bool
	ProvisionErr   error
	ProvisionDelay time.Duration
	Versions       core.DetectedVersions
	// FailLabelSubstr, when non-empty, fails Provision for any environment whose
	// derived seed label contains it. Because per-test env seeds carry the test
	// name (e.g. "test:canary/provision_env_fail/env"), this lets a single test
	// be steered to env_failed under the fake/dry-run provider by naming
	// convention, with no test-specific wiring.
	FailLabelSubstr string

	// OnProvision / OnTeardown are optional synchronous hooks for tests.
	OnProvision func(*Env)
	OnTeardown  func(*Env)

	provisioned       atomic.Int64
	sharedProvisioned atomic.Int64
	teardowns         atomic.Int64
	live              atomic.Int64
	peakLive          atomic.Int64

	mu   sync.Mutex
	next int
}

// Provision returns a fake environment, honoring the configured delay/failure.
func (p *Provider) Provision(ctx context.Context, spec core.EnvSpec, seed *core.Seed) (core.Environment, error) {
	if p.ProvisionDelay > 0 {
		select {
		case <-time.After(p.ProvisionDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.FailLabelSubstr != "" && seed != nil && strings.Contains(seed.Label(), p.FailLabelSubstr) {
		return nil, fmt.Errorf("fake provision failed for %s", seed.Label())
	}
	if p.FailProvision {
		if p.ProvisionErr != nil {
			return nil, p.ProvisionErr
		}
		return nil, fmt.Errorf("fake provision failed")
	}
	p.provisioned.Add(1)
	if spec.Sharing == core.Shared {
		p.sharedProvisioned.Add(1)
	}
	n := p.live.Add(1)
	for {
		old := p.peakLive.Load()
		if n <= old || p.peakLive.CompareAndSwap(old, n) {
			break
		}
	}
	p.mu.Lock()
	p.next++
	id := fmt.Sprintf("fake-env-%d", p.next)
	p.mu.Unlock()
	e := &Env{id: id, versions: p.Versions}
	if p.OnProvision != nil {
		p.OnProvision(e)
	}
	return e, nil
}

// Teardown records that teardown ran and decrements the live gauge.
func (p *Provider) Teardown(ctx context.Context, env core.Environment) error {
	p.teardowns.Add(1)
	p.live.Add(-1)
	if e, ok := env.(*Env); ok && p.OnTeardown != nil {
		p.OnTeardown(e)
	}
	return nil
}

// Attach returns a fresh fake environment for a reference id.
func (p *Provider) Attach(ctx context.Context, ref string) (core.Environment, error) {
	return &Env{id: ref, versions: p.Versions}, nil
}

// TeardownCount is how many times Teardown ran.
func (p *Provider) TeardownCount() int { return int(p.teardowns.Load()) }

// ProvisionCount is how many environments were successfully provisioned.
func (p *Provider) ProvisionCount() int { return int(p.provisioned.Load()) }

// SharedProvisionCount is how many SHARED-shaped environments were provisioned.
func (p *Provider) SharedProvisionCount() int { return int(p.sharedProvisioned.Load()) }

// PeakLive is the high-water mark of concurrently live (provisioned, not yet
// torn down) environments.
func (p *Provider) PeakLive() int { return int(p.peakLive.Load()) }

// Env is a fake environment.
type Env struct {
	id       string
	versions core.DetectedVersions
}

// ID is the environment's stable identifier (also its Handles().Namespace).
func (e *Env) ID() string { return e.id }

func (e *Env) ControllerURL() string                            { return "fake://" + e.id }
func (e *Env) Versions() core.DetectedVersions                  { return e.versions }
func (e *Env) Exec() exec.RemoteExecutor                        { return nil }
func (e *Env) Handles() core.EnvHandles                         { return core.EnvHandles{Namespace: e.id} }
func (e *Env) Collect(context.Context, core.ArtifactSink) error { return nil }
