package rest_test

import (
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/testenv"
)

func TestUsageStatsSetting(t *testing.T) {
	e := testenv.Start(t, testenv.Options{})
	st := obj(okData(t, e, "GET", "/settings/usage-stats", nil, 200))
	// On, with nowhere to send: a test server is no release and has no URL.
	if st["enabled"] != true || st["forced_off"] != false || st["sends"] != false {
		t.Errorf("default: %v", st)
	}
	st = obj(okData(t, e, "PATCH", "/settings/usage-stats", map[string]bool{"enabled": false}, 200))
	if st["enabled"] != false || st["forced_off"] != false || st["by"] != "web UI" {
		t.Errorf("turned off: %v", st)
	}
	if kept := obj(okData(t, e, "GET", "/settings/usage-stats", nil, 200)); kept["enabled"] != false {
		t.Errorf("not kept: %v", kept)
	}
	st = obj(okData(t, e, "PATCH", "/settings/usage-stats", map[string]bool{"enabled": true}, 200))
	if st["enabled"] != true || st["by"] != nil {
		t.Errorf("turned on: %v", st)
	}
	wantErr(t, e, "PATCH", "/settings/usage-stats", map[string]string{}, 400, "VALIDATION_ERROR")
	wantErr(t, e, "PATCH", "/settings/usage-stats", map[string]string{"enabled": "yes"}, 400, "INVALID_JSON")
}

func TestUsageStatsForcedOff(t *testing.T) {
	e := testenv.Start(t, testenv.Options{CallHome: &config.CallHome{OffBy: config.OffByYugabyteDB}})
	st := obj(okData(t, e, "PATCH", "/settings/usage-stats", map[string]bool{"enabled": true}, 200))
	if st["enabled"] != false || st["forced_off"] != true || st["by"] != "YB_DISABLE_CALLHOME" {
		t.Errorf("forced off: %v", st)
	}
}
