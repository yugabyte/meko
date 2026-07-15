package aws

import (
	"strings"
	"testing"
	"time"
)

// fakeGetter is a minimal ConfigGetter backed by a string map (only the keys
// ResolveCreds reads matter here).
type fakeGetter map[string]string

func (f fakeGetter) GetString(key, def string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return def
}
func (f fakeGetter) GetStringSlice(string, []string) []string        { return nil }
func (f fakeGetter) GetDuration(string, time.Duration) time.Duration { return 0 }

func TestResolveCredsAmbientByDefault(t *testing.T) {
	access, secret, err := ResolveCreds(fakeGetter{}, nil)
	if err != nil || access != "" || secret != "" {
		t.Errorf("no secret names → ambient; got access=%q secret=%q err=%v", access, secret, err)
	}
}

func TestResolveCredsResolvesByName(t *testing.T) {
	c := fakeGetter{
		"apps.cloud.aws.access_key_secret": "aws_access",
		"apps.cloud.aws.secret_key_secret": "aws_secret",
	}
	vals := map[string]string{"aws_access": "AKIA123", "aws_secret": "shhh"}
	resolve := func(name string) (string, error) { return vals[name], nil }

	access, secret, err := ResolveCreds(c, resolve)
	if err != nil {
		t.Fatalf("ResolveCreds: %v", err)
	}
	if access != "AKIA123" || secret != "shhh" {
		t.Errorf("resolved creds = %q/%q, want AKIA123/shhh", access, secret)
	}
}

func TestResolveCredsPartialIsError(t *testing.T) {
	c := fakeGetter{"apps.cloud.aws.access_key_secret": "only_one"}
	if _, _, err := ResolveCreds(c, func(string) (string, error) { return "x", nil }); err == nil {
		t.Errorf("setting exactly one of the credential secrets should error")
	}
}

func TestResolveCredsMissingResolver(t *testing.T) {
	c := fakeGetter{
		"apps.cloud.aws.access_key_secret": "a",
		"apps.cloud.aws.secret_key_secret": "b",
	}
	if _, _, err := ResolveCreds(c, nil); err == nil {
		t.Errorf("named creds with a nil resolver should error, not silently fall back")
	}
}

func TestResolveCredsResolutionError(t *testing.T) {
	c := fakeGetter{
		"apps.cloud.aws.access_key_secret": "a",
		"apps.cloud.aws.secret_key_secret": "b",
	}
	resolve := func(name string) (string, error) {
		if name == "b" {
			return "", errMissing
		}
		return "ok", nil
	}
	if _, _, err := ResolveCreds(c, resolve); err == nil {
		t.Errorf("a secret that fails to resolve should surface as an error")
	}
}

var errMissing = &resolveErr{"env not set"}

type resolveErr struct{ s string }

func (e *resolveErr) Error() string { return e.s }

func TestCredEnvInjection(t *testing.T) {
	// No creds → nothing injected (ambient chain).
	if env := (Config{}).credEnv(); env != nil {
		t.Errorf("no creds should inject nothing, got %v", env)
	}
	// Partial → nothing (defensive; ResolveCreds already rejects this).
	if env := (Config{AccessKey: "a"}).credEnv(); env != nil {
		t.Errorf("partial creds should inject nothing, got %v", env)
	}
	// Both → the two standard AWS env vars, in the child env only.
	env := Config{AccessKey: "AKIA", SecretKey: "sk"}.credEnv()
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "AWS_ACCESS_KEY_ID=AKIA") || !strings.Contains(joined, "AWS_SECRET_ACCESS_KEY=sk") {
		t.Errorf("credEnv = %v, want both AWS_* vars", env)
	}
}
