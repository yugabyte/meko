package ampstack

import (
	"strings"
	"testing"
)

// TestYugabytedManifestGflags: extra master/tserver gflags land on the
// yugabyted start line; the tserver auth default is always kept.
func TestYugabytedManifestGflags(t *testing.T) {
	m := yugabytedManifest("ns", "img", "10Gi", "",
		"enable_load_balancing=false", "yb_num_shards_per_tserver=4")
	if !strings.Contains(m, `--tserver_flags="ysql_enable_auth=false,yb_num_shards_per_tserver=4"`) {
		t.Errorf("tserver flags not merged with the auth default:\n%s", m)
	}
	if !strings.Contains(m, `--master_flags="enable_load_balancing=false"`) {
		t.Errorf("master flags missing:\n%s", m)
	}

	m = yugabytedManifest("ns", "img", "10Gi", "", "", "")
	if !strings.Contains(m, `--tserver_flags="ysql_enable_auth=false"`) || strings.Contains(m, "--master_flags") {
		t.Errorf("no-gflags manifest changed the historical start line:\n%s", m)
	}
}

// TestJoinGflags: empty parts are dropped, order preserved.
func TestJoinGflags(t *testing.T) {
	if got := joinGflags("", "a=1", " ", "b=2"); got != "a=1,b=2" {
		t.Errorf("joinGflags = %q, want a=1,b=2", got)
	}
	if got := joinGflags("", ""); got != "" {
		t.Errorf("joinGflags all-empty = %q, want empty", got)
	}
}
