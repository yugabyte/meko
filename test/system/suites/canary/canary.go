// Package canary is the framework's self-test suite: tests that deliberately
// pass, fail, panic, hang, fail to provision, and skip by version. Its own Go
// test asserts each lands with exactly its intended status — the self-test gate
// for the engine's status taxonomy, teardown-always, and lease discipline.
//
// The suite is excluded from normal selection: `ampqa run` only runs it when
// --suite canary is given explicitly.
package canary

import (
	"fmt"
	"time"

	"github.com/yugabyte/yb-amp/test/system/framework/core"
)

// EnvFailSubstr is the seed-label marker the fake/dry-run provider matches to
// fail provisioning for the env-fail canary (fake.Provider.FailLabelSubstr).
const EnvFailSubstr = "env_fail"

func init() {
	core.Register(core.Test{
		Name:        "canary/pass",
		Suite:       "canary",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Description: "Self-test: always passes (asserts the passed status).",
		Run:         func(*core.T) error { return nil },
	})

	core.Register(core.Test{
		Name:        "canary/fail",
		Suite:       "canary",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Description: "Self-test: always returns an error (asserts the failed status).",
		Run: func(t *core.T) error {
			return fmt.Errorf("deliberate canary failure")
		},
	})

	core.Register(core.Test{
		Name:        "canary/panic",
		Suite:       "canary",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Description: "Self-test: panics in its body (asserts the broken status).",
		Run:         func(*core.T) error { panic("deliberate canary panic") },
	})

	core.Register(core.Test{
		Name:        "canary/hang",
		Suite:       "canary",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Description: "Self-test: outlasts its timeout (asserts the timed-out status + watchdog cancel).",
		Timeout:     150 * time.Millisecond,
		Run: func(t *core.T) error {
			// Outlast the timeout; exit promptly when the watchdog cancels ctx.
			select {
			case <-t.Done():
			case <-time.After(30 * time.Second):
			}
			return nil
		},
	})

	// provision_env_fail carries EnvFailSubstr in its name so its per-test env
	// seed label triggers the fake provider's provision failure. Exclusive so it
	// provisions its own environment (whose seed label carries the test name).
	core.Register(core.Test{
		Name:        "canary/provision_" + EnvFailSubstr,
		Suite:       "canary",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Exclusive, Shape: core.SingleContainer},
		Description: "Self-test: forces a provision failure (asserts the env-failed status).",
		Run:         func(*core.T) error { return nil },
	})

	core.Register(core.Test{
		Name:        "canary/skip_version",
		Suite:       "canary",
		Category:    core.Functional,
		Env:         core.EnvSpec{Sharing: core.Shared, Shape: core.SingleContainer},
		Description: "Self-test: gated out by a version constraint (asserts the version skip).",
		Versions:    core.VersionSpec{MaxProduct: "0.0.0.1"},
		Run:         func(*core.T) error { return nil },
	})
}
