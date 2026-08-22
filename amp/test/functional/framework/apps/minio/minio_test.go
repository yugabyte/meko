package minio

import (
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec/k8s"
	"github.com/yugabyte/amp/test/functional/framework/kubemeta"
)

// The janitor finds every ampqa-created object by these labels, so a pod that
// drops them leaks past teardown.
func TestManifestsCarryTheJanitorLabels(t *testing.T) {
	for name, m := range map[string]string{
		"server":  serverManifest(Spec{Name: "minio-1", Bucket: "b"}),
		"oneShot": oneShotManifest("minio-1-mb", "minio/mc:latest", "true"),
	} {
		if !strings.Contains(m, kubemeta.TestLabel+": \""+kubemeta.TestLabelValue+"\"") {
			t.Errorf("%s manifest is missing the %s label:\n%s", name, kubemeta.TestLabel, m)
		}
		if !strings.Contains(m, "ampqa: \"1\"") {
			t.Errorf("%s manifest is missing the ampqa label:\n%s", name, m)
		}
	}
}

// The Service selects the server pod by app label; a mismatch yields a Service
// with no endpoints, which surfaces as an unreachable backup location rather
// than as a deploy failure.
func TestServerManifestServiceSelectorMatchesThePod(t *testing.T) {
	m := serverManifest(Spec{Name: "minio-xyz", Bucket: "b"})
	pod, svc, ok := strings.Cut(m, "---\n")
	if !ok {
		t.Fatalf("expected a pod and a service document:\n%s", m)
	}
	if !strings.Contains(pod, "    app: minio-xyz\n") {
		t.Errorf("pod carries no app label:\n%s", pod)
	}
	if !strings.Contains(svc, "  selector:\n    app: minio-xyz\n") {
		t.Errorf("service selector does not match the pod:\n%s", svc)
	}
	if !strings.Contains(svc, "targetPort: 9000") {
		t.Errorf("service does not target the MinIO port:\n%s", svc)
	}
}

// The root credentials must reach the server as env, or every backup 403s.
func TestServerManifestCarriesRootCredentialsAndReadiness(t *testing.T) {
	m := serverManifest(Spec{Name: "minio-1", Bucket: "b", ServerImage: "minio/minio:RELEASE.X"})
	for _, want := range []string{
		"image: minio/minio:RELEASE.X",
		"MINIO_ROOT_USER",
		AccessKey,
		"MINIO_ROOT_PASSWORD",
		SecretKey,
		"/minio/health/ready",
	} {
		if !strings.Contains(m, want) {
			t.Errorf("server manifest missing %q:\n%s", want, m)
		}
	}
}

// Regression: the bootstrap ran once and died on "connection refused" because
// kube-proxy had not published the Service endpoint yet, which happens some
// moment after the server pod reports Ready. The script must retry.
func TestBucketScriptRetriesUntilTheServiceAnswers(t *testing.T) {
	script := bucketScript(Spec{Bucket: "ampqa-backups"}, "http://minio-1.ns.svc.cluster.local:9000")
	for _, want := range []string{"for i in $(seq 1 60)", "sleep 2", "exit 0", "exit 1"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q, so a single failed attempt aborts the run:\n%s", want, script)
		}
	}
	if !strings.Contains(script, "mc mb --ignore-existing target/ampqa-backups") {
		t.Errorf("bucket create is not idempotent:\n%s", script)
	}
	// The in-pod retry must give up before the pod-level wait does, or the
	// failure surfaces as an opaque timeout instead of mc's own error.
	if budget := time.Duration(bucketAttempts) * bucketRetry; budget >= 5*time.Minute {
		t.Errorf("in-pod retry budget %s exceeds the default ReadyTimeout", budget)
	}
}

// The bootstrap script is embedded in YAML as a quoted scalar, so an unescaped
// quote would produce a manifest kubectl rejects.
func TestOneShotManifestQuotesTheScript(t *testing.T) {
	m := oneShotManifest("mb", "minio/mc:latest", `mc alias set target "http://h:9000" k s`)
	if !strings.Contains(m, `command: ["sh", "-c", "mc alias set target \"http://h:9000\" k s"]`) {
		t.Errorf("script not escaped into the command:\n%s", m)
	}
}

// Endpoint is the address YBA records in the storage config, so it must be the
// Service's namespaced cluster DNS name — a bare pod name resolves nowhere
// from the YBA pod.
func TestEndpointIsTheInClusterServiceURL(t *testing.T) {
	s := &Server{kube: k8s.New(k8s.Config{Namespace: "yb-ns"}), name: "minio-1"}
	if got, want := s.Endpoint(), "http://minio-1.yb-ns.svc.cluster.local:9000"; got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
}
