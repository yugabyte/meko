package config

import (
	"testing"
	"time"
)

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

func TestSystemURLWinsOverMainURL(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"YUGABYTEDB_URL": "main"}))
	if err != nil || c.DSN != "main" {
		t.Fatalf("got %q, %v", c.DSN, err)
	}
	c, err = FromEnv(env(map[string]string{"YUGABYTEDB_URL": "main", "YUGABYTEDB_SYSTEM_URL": "system"}))
	if err != nil || c.DSN != "system" {
		t.Fatalf("got %q, %v", c.DSN, err)
	}
}

func TestMissingDatabaseURLIsAnError(t *testing.T) {
	if _, err := FromEnv(env(nil)); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDefaultsMatchTheDesign(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"YUGABYTEDB_URL": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Consumer != "analytics" || c.Schema != "meko_system" || c.BatchSize != 500 || c.SafetyLag != 3*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.BatchTimeout != 15*time.Second {
		t.Fatalf("batch timeout = %v, want 15s (lease 30s - renew 10s - a sixth of the lease)", c.BatchTimeout)
	}
	if c.LookBackWindow != 5*time.Minute || c.LookBackEvery != 30*time.Second {
		t.Fatalf("unexpected look-back defaults: %+v", c)
	}
	if c.Lease != 30*time.Second || c.Renew != 10*time.Second || c.DeadLetterDays != 14 || len(c.Shards) != 16 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestSettingsAreParsedAndClamped(t *testing.T) {
	cases := []struct {
		name, raw string
		get       func(Config) any
		want      any
	}{
		{"BATCH_SIZE", "50", func(c Config) any { return c.BatchSize }, 50},
		{"BATCH_SIZE", "0", func(c Config) any { return c.BatchSize }, 1},
		{"BATCH_SIZE", "999999", func(c Config) any { return c.BatchSize }, 5000},
		{"BATCH_SIZE", "lots", func(c Config) any { return c.BatchSize }, 500},
		{"MAX_SHARDS", "99", func(c Config) any { return c.MaxShards }, 16},
		{"SAFETY_LAG_SECONDS", "0.5", func(c Config) any { return c.SafetyLag }, 500 * time.Millisecond},
		{"LOOKBACK_WINDOW_SECONDS", "600", func(c Config) any { return c.LookBackWindow }, 10 * time.Minute},
		{"LOOKBACK_WINDOW_SECONDS", "1", func(c Config) any { return c.LookBackWindow }, 30 * time.Second},
		{"LOOKBACK_INTERVAL_SECONDS", "1", func(c Config) any { return c.LookBackEvery }, 5 * time.Second},
		{"LOOKBACK_INTERVAL_SECONDS", "x", func(c Config) any { return c.LookBackEvery }, 30 * time.Second},
		{"POLL_MIN_MS", "100", func(c Config) any { return c.PollMin }, 100 * time.Millisecond},
		{"HTTP_PORT", "0", func(c Config) any { return c.HTTPPort }, 0},
	}
	for _, tc := range cases {
		c, err := FromEnv(env(map[string]string{"YUGABYTEDB_URL": "x", Prefix + tc.name: tc.raw}))
		if err != nil {
			t.Fatal(err)
		}
		if got := tc.get(c); got != tc.want {
			t.Errorf("%s=%s: got %v, want %v", tc.name, tc.raw, got, tc.want)
		}
	}
}

func TestRenewalIsNeverSlowerThanHalfTheLease(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"YUGABYTEDB_URL": "x", Prefix + "LEASE_SECONDS": "20", Prefix + "RENEW_SECONDS": "60"}))
	if err != nil || c.Renew != 10*time.Second {
		t.Fatalf("renew = %v, %v", c.Renew, err)
	}
}

func TestSchemaMustBeAPlainIdentifier(t *testing.T) {
	for _, bad := range []string{"a;drop table x", `a"b`, "1abc", "a b"} {
		if _, err := FromEnv(env(map[string]string{"YUGABYTEDB_URL": "x", Prefix + "SCHEMA": bad})); err == nil {
			t.Errorf("schema %q was accepted", bad)
		}
	}
}

func TestABatchCannotOutliveItsLease(t *testing.T) {
	for _, tc := range []struct {
		lease, renew string
		want         time.Duration
	}{
		{"30", "10", 15 * time.Second},
		{"60", "10", 40 * time.Second},
		{"5", "1", 3166666667 * time.Nanosecond},
		{"5", "5", 2 * time.Second}, // a floor, so a tiny lease does not give a batch no time at all
	} {
		c, err := FromEnv(env(map[string]string{
			"YUGABYTEDB_URL": "x", Prefix + "LEASE_SECONDS": tc.lease, Prefix + "RENEW_SECONDS": tc.renew,
		}))
		if err != nil {
			t.Fatal(err)
		}
		if c.BatchTimeout != tc.want {
			t.Errorf("lease %s renew %s: batch timeout = %v, want %v", tc.lease, tc.renew, c.BatchTimeout, tc.want)
		}
		if c.BatchTimeout >= c.Lease-c.Renew && c.Lease-c.Renew > 2*time.Second {
			t.Errorf("lease %s renew %s: batch timeout %v is not below lease - renew",
				tc.lease, tc.renew, c.BatchTimeout)
		}
	}
}

func TestAnIdlePollNeverOutlastsALeaseRenewal(t *testing.T) {
	cases := []struct {
		name         string
		kv           map[string]string
		wantMin, max time.Duration
	}{
		{"defaults", nil, 500 * time.Millisecond, 2 * time.Second},
		{"a poll longer than the renewal is clamped to it", map[string]string{"POLL_MAX_MS": "60000"},
			500 * time.Millisecond, 10 * time.Second},
		{"the minimum never exceeds the maximum", map[string]string{"POLL_MIN_MS": "20000", "POLL_MAX_MS": "5000"},
			5 * time.Second, 5 * time.Second},
	}
	for _, tc := range cases {
		kv := map[string]string{"YUGABYTEDB_URL": "x"}
		for k, v := range tc.kv {
			kv[Prefix+k] = v
		}
		c, err := FromEnv(env(kv))
		if err != nil {
			t.Fatal(err)
		}
		if c.PollMin != tc.wantMin || c.PollMax != tc.max {
			t.Errorf("%s: poll min/max = %v/%v, want %v/%v", tc.name, c.PollMin, c.PollMax, tc.wantMin, tc.max)
		}
	}
}
