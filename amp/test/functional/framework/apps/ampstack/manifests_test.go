package ampstack

import (
	"strings"
	"testing"
)

// TestYugabytedManifestGflags: extra master/tserver gflags land on the
// yugabyted start line; the tserver auth default is always kept.
func TestYugabytedManifestGflags(t *testing.T) {
	m := yugabytedManifest("ns", "img", "10Gi", "",
		"enable_load_balancing=false", "yb_num_shards_per_tserver=4", "")
	if !strings.Contains(m, `--tserver_flags="ysql_enable_auth=false,pgsql_proxy_bind_address=0.0.0.0:5433,yb_num_shards_per_tserver=4"`) {
		t.Errorf("tserver flags not merged with the auth default:\n%s", m)
	}
	if !strings.Contains(m, `--master_flags="enable_load_balancing=false"`) {
		t.Errorf("master flags missing:\n%s", m)
	}

	m = yugabytedManifest("ns", "img", "10Gi", "", "", "", "")
	if !strings.Contains(m, `--tserver_flags="ysql_enable_auth=false,pgsql_proxy_bind_address=0.0.0.0:5433"`) || strings.Contains(m, "--master_flags") {
		t.Errorf("no-gflags manifest changed the historical start line:\n%s", m)
	}
}

// TestYugabytedManifestImagePullSecret: a non-empty pull-secret name emits an
// imagePullSecrets entry on the pod spec (private YB image); an empty name omits
// it entirely (public-image path unchanged).
func TestYugabytedManifestImagePullSecret(t *testing.T) {
	m := yugabytedManifest("ns", "quay.io/yugabyte/yugabyte-itest:2.25.1.0-b1", "10Gi", "", "", "", "pull-secret")
	if !strings.Contains(m, "imagePullSecrets:") || !strings.Contains(m, "- name: pull-secret") {
		t.Errorf("private-image manifest missing imagePullSecrets:\n%s", m)
	}

	if m := yugabytedManifest("ns", "img", "10Gi", "", "", "", ""); strings.Contains(m, "imagePullSecrets") {
		t.Errorf("empty pull-secret name still emitted imagePullSecrets:\n%s", m)
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

// The yugabyted PVC pins a class instead of the cluster default (zone-pinning).
func TestYugabytedManifestStorageClass(t *testing.T) {
	m := yugabytedManifest("ns", "img", "10Gi", DefaultYBStorageClass, "", "", "")
	if !strings.Contains(m, "storageClassName: "+DefaultYBStorageClass) {
		t.Errorf("manifest missing storageClassName %q:\n%s", DefaultYBStorageClass, m)
	}
	if m := yugabytedManifest("ns", "img", "10Gi", "", "", "", ""); strings.Contains(m, "storageClassName") {
		t.Errorf("empty class must leave the PVC on the cluster default:\n%s", m)
	}
}
