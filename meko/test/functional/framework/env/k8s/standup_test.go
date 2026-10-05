package k8s

import (
	"context"
	"strings"
	"testing"
)

func TestStandUpRefusesWhatItCannotBuild(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cases := map[string]struct {
		overrides []string
		want      string
	}{
		"attach namespace": {[]string{"env.k8s.namespace=meko-byoc-dev"}, "does not attach"},
		"no context":       {nil, "no kube context"},
		"no credential":    {[]string{"env.k8s.context=itest-dev"}, "Quay credential"},
	}
	for name, tc := range cases {
		_, err := StandUp(context.Background(), load(t, tc.overrides...))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

func TestDiscoverContextPrefersTheConfiguredOne(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := DiscoverContext(load(t, "env.k8s.context=itest-dev")); got != "itest-dev" {
		t.Errorf("DiscoverContext = %q", got)
	}
	if got := DiscoverContext(load(t)); got != "" {
		t.Errorf("DiscoverContext with no kubectl = %q, want empty", got)
	}
}
