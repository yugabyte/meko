package config

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/controller/pkg/pglease"
)

// The lease timings reach pglease.Config, which validates them again — so anything
// Load accepts and pglease rejects becomes a startup failure naming a package the
// operator never configured. These pin Load to the stricter set.

func TestLoadRejectsUnrenewableLeaseTimings(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{{
		// pglease needs RenewEvery < TTL - SafetyMargin, not merely < TTL, so a
		// 29s/30s pair is accepted by the naive rule and rejected by the real one.
		name: "renew interval inside the safety margin",
		args: []string{"--task-lease-ttl", "30s", "--task-lease-renew-interval", "29s"},
		want: "safety margin",
	}, {
		name: "renew interval above the TTL",
		args: []string{"--task-lease-ttl", "10s", "--task-lease-renew-interval", "30s"},
		want: "safety margin",
	}, {
		name: "TTL above the pglease maximum",
		args: []string{"--task-lease-ttl", "10m"},
		want: "task lease TTL",
	}, {
		name: "lock wait not positive",
		args: []string{"--task-lock-wait", "0"},
		want: "task lock wait",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(append([]string{"--database-url", "postgres://localhost/db"}, tc.args...))
			if err == nil {
				t.Fatalf("Load accepted %v; pglease would reject it at NewPgLocks", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// The shipped defaults must satisfy pglease, or every controller aborts one safety
// margin after its first missed renewal.
func TestDefaultLeaseTimingsAreRenewable(t *testing.T) {
	c, err := Load([]string{"--database-url", "postgres://localhost/db"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := pglease.Config{
		TTL:          c.Task.LeaseTTL,
		RenewEvery:   c.Task.RenewInterval,
		RenewTimeout: pglease.DefaultRenewTimeout,
		SafetyMargin: pglease.DefaultSafetyMargin,
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the default lease timings (%s/%s) are invalid: %v",
			c.Task.LeaseTTL, c.Task.RenewInterval, err)
	}
	if c.Task.LockWait <= 0 {
		t.Errorf("default LockWait = %s, want positive", c.Task.LockWait)
	}
}
