package mekoapi

import (
	"slices"
	"testing"
)

func TestInternalLeaks(t *testing.T) {
	cases := []struct {
		msg  string
		want []string
	}{
		{`failed to cleanup langfuse project before deleting datapack: delete langfuse project cmurmaz9301soi607n74klcv0: send request: Delete "http://k8s-mekolang-langfuse-5d9cc6e6db-7d950f5a10c5ea10.elb.us-east-2.amazonaws.com:3000/api/public/projects/cmurmaz9301soi607n74klcv0": context deadline exceeded`,
			[]string{"k8s-mekolang-langfuse-5d9cc6e6db-7d950f5a10c5ea10.elb.us-east-2.amazonaws.com:3000"}},
		{`send request: Delete "http://langfuse-web.mekoqa-st-5416dd7b.svc.cluster.local.:3000/api/public/projects/x": dial tcp 34.118.234.111:3000: connect: connection refused`,
			[]string{"langfuse-web.mekoqa-st-5416dd7b.svc.cluster.local.:3000", "34.118.234.111:3000"}},
		{"name is required", nil},
		{"Must have at least 3 characters", nil},
		{"datapack 00000000-0000-4000-8000-000000000000 not found", nil},
		{"token lacks required scope \"write\"", nil},
	}
	for _, c := range cases {
		if got := InternalLeaks(c.msg); !slices.Equal(got, c.want) {
			t.Errorf("InternalLeaks(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}
