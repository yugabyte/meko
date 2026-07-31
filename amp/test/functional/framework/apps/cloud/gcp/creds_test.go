package gcp

import (
	"testing"
	"time"
)

// fakeGetter is a minimal ConfigGetter backed by a string map (only the keys
// ResolveServiceAccountKeyFile reads matter here).
type fakeGetter map[string]string

func (f fakeGetter) GetString(key, def string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return def
}
func (f fakeGetter) GetStringSlice(string, []string) []string        { return nil }
func (f fakeGetter) GetDuration(string, time.Duration) time.Duration { return 0 }

func TestResolveServiceAccountAmbientByDefault(t *testing.T) {
	path, err := ResolveServiceAccountKeyFile(fakeGetter{}, nil)
	if err != nil || path != "" {
		t.Errorf("no secret name → ambient; got path=%q err=%v", path, err)
	}
}

func TestResolveServiceAccountResolvesByName(t *testing.T) {
	c := fakeGetter{"apps.cloud.gcp.service_account_secret": "gcp_key"}
	resolve := func(name string) (string, error) {
		if name == "gcp_key" {
			return "/secure/sa.json", nil
		}
		return "", nil
	}
	path, err := ResolveServiceAccountKeyFile(c, resolve)
	if err != nil {
		t.Fatalf("ResolveServiceAccountKeyFile: %v", err)
	}
	if path != "/secure/sa.json" {
		t.Errorf("resolved path = %q, want /secure/sa.json", path)
	}
}

func TestResolveServiceAccountMissingResolver(t *testing.T) {
	c := fakeGetter{"apps.cloud.gcp.service_account_secret": "gcp_key"}
	if _, err := ResolveServiceAccountKeyFile(c, nil); err == nil {
		t.Errorf("named secret with a nil resolver should error, not silently fall back to ambient")
	}
}

func TestResolveServiceAccountResolutionError(t *testing.T) {
	c := fakeGetter{"apps.cloud.gcp.service_account_secret": "gcp_key"}
	resolve := func(name string) (string, error) {
		return "", errMissing
	}
	if _, err := ResolveServiceAccountKeyFile(c, resolve); err == nil {
		t.Errorf("a secret that fails to resolve should surface as an error")
	}
}

var errMissing = &resolveErr{"env not set"}

type resolveErr struct{ s string }

func (e *resolveErr) Error() string { return e.s }
