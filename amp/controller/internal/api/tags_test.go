package api

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateTags(t *testing.T) {
	many := map[string]string{}
	for i := range maxTags + 1 {
		many[fmt.Sprintf("k%d", i)] = "v"
	}
	cases := []struct {
		name    string
		tags    map[string]string
		wantErr string
	}{
		{name: "none", tags: nil},
		{name: "plain", tags: map[string]string{"team": "web", "cost-center": "42"}},
		{name: "prefixed key", tags: map[string]string{"acme.com/owner": "jdoe"}},
		{name: "empty value", tags: map[string]string{"canary": ""}},
		{name: "63-char name and value", tags: map[string]string{strings.Repeat("a", 63): strings.Repeat("b", 63)}},

		{name: "too many", tags: many, wantErr: "at most 50"},
		{name: "empty key", tags: map[string]string{"": "v"}, wantErr: "name must be"},
		{name: "space in key", tags: map[string]string{"bad key": "v"}, wantErr: "name must be"},
		{name: "key ends in a dash", tags: map[string]string{"team-": "v"}, wantErr: "name must be"},
		{name: "64-char name", tags: map[string]string{strings.Repeat("a", 64): "v"}, wantErr: "name must be"},
		{name: "empty name after prefix", tags: map[string]string{"acme.com/": "v"}, wantErr: "name must be"},
		{name: "uppercase prefix", tags: map[string]string{"Acme.com/owner": "v"}, wantErr: "prefix must be"},
		{name: "two slashes", tags: map[string]string{"acme.com/a/b": "v"}, wantErr: "name must be"},
		{name: "64-char value", tags: map[string]string{"team": strings.Repeat("b", 64)}, wantErr: "value"},
		{name: "space in value", tags: map[string]string{"team": "has space"}, wantErr: "value"},

		{name: "selector key", tags: map[string]string{"app.kubernetes.io/instance": "x"}, wantErr: "chart sets"},
		{name: "helm key", tags: map[string]string{"helm.sh/chart": "x"}, wantErr: "chart sets"},
		{name: "amp key", tags: map[string]string{"amp.yugabyte.com/ingress-shard": "x"}, wantErr: "reserved"},
		{name: "amp key not in use yet", tags: map[string]string{"amp.yugabyte.com/anything": "x"}, wantErr: "reserved"},
		{name: "amp subdomain", tags: map[string]string{"ingress.amp.yugabyte.com/x": "y"}, wantErr: "reserved"},
		// Only what collides is refused: the rest of these domains is open.
		{name: "recommended label", tags: map[string]string{"app.kubernetes.io/part-of": "shop"}},
		{name: "k8s domain", tags: map[string]string{"node.kubernetes.io/role": "x"}},
		{name: "helm domain", tags: map[string]string{"helm.sh/other": "x"}},
		// A domain that merely ends in the same letters is someone else's.
		{name: "lookalike domain", tags: map[string]string{"notamp.yugabyte.com/x": "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTags(tc.tags)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateTags(%v) = %v, want nil", tc.tags, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTags(%v) = %v, want an error containing %q", tc.tags, err, tc.wantErr)
			}
		})
	}
}
