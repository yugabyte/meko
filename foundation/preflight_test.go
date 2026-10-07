package foundation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{in: "v3.20.0", major: 3, minor: 20, ok: true},
		{in: "3.11.1", major: 3, minor: 11, ok: true},
		{in: " v3.9.4\n", major: 3, minor: 9, ok: true},
		{in: "v3", ok: false},
		{in: "not-a-version", ok: false},
	} {
		major, minor, ok := parseVersion(tc.in)
		if ok != tc.ok || (ok && (major != tc.major || minor != tc.minor)) {
			t.Errorf("parseVersion(%q) = %d,%d,%v; want %d,%d,%v", tc.in, major, minor, ok, tc.major, tc.minor, tc.ok)
		}
	}
}

// A managed cluster reports its minor version as "27+", which a plain Atoi
// rejects — and rejecting it would fail preflight on exactly the clusters this
// runs against.
func TestParseNumberToleratesTheManagedClusterPlusSuffix(t *testing.T) {
	if n, ok := parseNumber("27+"); !ok || n != 27 {
		t.Errorf(`parseNumber("27+") = %d,%v; want 27,true`, n, ok)
	}
	if _, ok := parseNumber("+"); ok {
		t.Error(`parseNumber("+") reported ok`)
	}
}

func TestCheckHelmVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     Result
		wantErr string
	}{{
		name: "current helm passes",
		out:  Result{Stdout: "v3.20.0"},
	}, {
		name:    "too old fails with the fix",
		out:     Result{Stdout: "v3.9.0"},
		wantErr: "upgrade helm",
	}, {
		// An unreadable version is not a reason to refuse to run: the install
		// itself is a better judge than a failed version probe.
		name: "unreadable version only warns",
		out:  Result{Stdout: "weird-build"},
	}, {
		name: "a failed probe only warns",
		out:  Result{ExitCode: 1, Stderr: "unknown flag"},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{{Match: "helm version", Result: tc.out}}}
			d, _ := testDeployer(t, rec)
			err := d.checkHelmVersion(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkHelmVersion: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCheckK8sVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stdout  string
		wantErr string
	}{{
		name:   "current cluster passes",
		stdout: `{"serverVersion":{"major":"1","minor":"31"}}`,
	}, {
		name:   "a managed cluster's plus suffix passes",
		stdout: `{"serverVersion":{"major":"1","minor":"27+"}}`,
	}, {
		name:    "too old fails",
		stdout:  `{"serverVersion":{"major":"1","minor":"24"}}`,
		wantErr: "needs >= 1.27",
	}, {
		name:   "unparseable only warns",
		stdout: `{"serverVersion":{}}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &Recorder{Responses: []Response{{Match: "version -o json", Result: Result{Stdout: tc.stdout}}}}
			d, _ := testDeployer(t, rec)
			err := d.checkK8sVersion(context.Background())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkK8sVersion: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestPreflightWarnsOnImmediateBinding: an Immediate storage class provisions
// the PVC before the pod is scheduled, which zone-pins the ~4.5 CPU yugaware
// pod and then fails to schedule it. A warning rather than an error, because a
// single-zone cluster is unaffected.
func TestPreflightWarnsOnImmediateBinding(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "helm version", Result: Result{Stdout: "v3.20.0"}},
		{Match: "version -o json", Result: Result{Stdout: `{"serverVersion":{"major":"1","minor":"31"}}`}},
		{Match: "get storageclass", Result: Result{Stdout: "Immediate"}},
	}}
	spec := loadExample(t, "foundation-cli.yaml")
	var logged []string
	d := New(spec, rec.Runner()).WithLogger(func(f string, a ...any) { logged = append(logged, f) })

	if err := d.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	var warned bool
	for _, l := range logged {
		if strings.Contains(l, "WaitForFirstConsumer") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no warning about Immediate binding; logged: %v", logged)
	}
}

// A missing storage class IS fatal: the universe's PVCs would never bind, and
// the failure would arrive as unschedulable pods much later.
func TestPreflightFailsOnMissingStorageClass(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "helm version", Result: Result{Stdout: "v3.20.0"}},
		{Match: "version -o json", Result: Result{Stdout: `{"serverVersion":{"major":"1","minor":"31"}}`}},
		{Match: "get storageclass", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
	}}
	d, _ := testDeployer(t, rec)
	err := d.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight passed with a missing storage class")
	}
	// The message has to name where a storage class can be set, since the
	// provider's zone value is the one that actually wins.
	if !strings.Contains(err.Error(), "kubernetesStorageClass") {
		t.Errorf("error %q does not name the field to fix", err)
	}
}

// TestPreflightFailsOnAMissingPullSecret: nothing here creates one, and a
// missing one is not a deploy error — the pods are admitted and sit in
// ImagePullBackOff, so without this the failure is a wait that never ends.
func TestPreflightFailsOnAMissingPullSecret(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "helm version", Result: Result{Stdout: "v3.20.0"}},
		{Match: "version -o json", Result: Result{Stdout: `{"serverVersion":{"major":"1","minor":"31"}}`}},
		{Match: "get storageclass", Result: Result{Stdout: "WaitForFirstConsumer"}},
		{Match: "get secret", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
	}}
	d, _ := testDeployer(t, rec)
	err := d.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight passed with a pull secret that does not exist")
	}
	if !strings.Contains(err.Error(), "quay-pull-secret") || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("error %q does not name the secret and what would go wrong", err)
	}
}

// TestPullSecretsIsOnlyYugawares: the DB image's pull secret is deliberately
// NOT checked here. YBA derives it from its own and propagates it into whatever
// namespace it deploys universe nodes into — which is the only thing that can
// work, since the YBProvider CRD carries no pull-secret CONTENT field (only
// kubeConfigSecret), so a CR could name a Secret but never put one there.
// Requiring the name in OUR namespace would fail specs that are correct.
func TestPullSecretsIsOnlyYugawares(t *testing.T) {
	spec := loadExample(t, "foundation-cli.yaml")
	if got := spec.PullSecrets(); len(got) != 1 || got[0] != "quay-pull-secret" {
		t.Fatalf("PullSecrets() = %v, want just yugaware's [quay-pull-secret]", got)
	}
	// Naming one on the universe must not add a requirement: that Secret has to
	// exist beside the DB pods, which may be in a namespace we never touch.
	spec.universe()["kubernetesOverrides"] = map[string]any{
		"Image": map[string]any{"pullSecretName": "somewhere-elses-secret"},
	}
	for _, name := range spec.PullSecrets() {
		if name == "somewhere-elses-secret" {
			t.Error("PullSecrets requires the DB image's secret in the foundation's namespace; " +
				"YBA propagates that one into the universe's namespace itself")
		}
	}
}

// TestPreflightNamesAMissingChartOrCrdPath turns a path typo into a named
// field rather than a helm or kubectl failure after the CRDs have already been
// applied.
func TestPreflightNamesAMissingChartOrCrdPath(t *testing.T) {
	dir := t.TempDir()
	chart := filepath.Join(dir, "yugaware-2026.1.1.tgz")
	if err := os.WriteFile(chart, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	crds := filepath.Join(dir, "crds")
	if err := os.Mkdir(crds, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name          string
		chart, crdDir string
		source        ChartSource
		wantErr       string
	}{
		{name: "both present, tarball and directory", chart: chart, crdDir: crds, source: ChartPath},
		{name: "chart missing", chart: filepath.Join(dir, "nope.tgz"), crdDir: crds, source: ChartPath,
			wantErr: "yba.chart"},
		{name: "crdPath missing", chart: chart, crdDir: filepath.Join(dir, "nope"), source: ChartPath,
			wantErr: "crdPath"},
		// A URL is helm's and kubectl's to fetch, so there is nothing local to
		// stat and no reason to fail.
		{name: "URLs are not checked", chart: "https://example.test/yugaware.tgz",
			crdDir: "https://example.test/crds.yaml", source: ChartPath},
		// Under repo, yba.chart is a chart NAME, not a path.
		{name: "a repo chart name is not a path", chart: "yugabytedb/yugaware", crdDir: crds, source: ChartRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := loadExample(t, "foundation-cli.yaml")
			spec.ChartSource = tc.source
			spec.YBA.Chart = tc.chart
			spec.CRDPath = tc.crdDir
			err := New(spec, (&Recorder{}).Runner()).WithLogger(func(string, ...any) {}).checkLocalPaths()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkLocalPaths: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
}

// TestPreflightFailsOnAMissingTelemetrySecret: a TelemetryProvider's credential
// is a Secret reference, and a missing one fails in the worst possible way — the
// operator parks the provider in Error with the reason only in YBA's own log,
// the universe's telemetry section never reconciles, and the universe itself
// looks entirely healthy while exporting nothing. Preflight is where that
// becomes a sentence instead of a silence.
func TestPreflightFailsOnAMissingTelemetrySecret(t *testing.T) {
	rec := &Recorder{Responses: []Response{
		{Match: "helm version", Result: Result{Stdout: "v3.20.0"}},
		{Match: "version -o json", Result: Result{Stdout: `{"serverVersion":{"major":"1","minor":"31"}}`}},
		{Match: "get storageclass", Result: Result{Stdout: "WaitForFirstConsumer"}},
		// The pull secret resolves; only the telemetry one is absent.
		{Match: "get secret quay-pull-secret", Result: Result{Stdout: "secret/quay-pull-secret"}},
		{Match: "get secret", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
	}}
	spec := loadExample(t, "resources.yaml")
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})

	err := d.Preflight(context.Background())
	if err == nil {
		t.Fatal("Preflight passed with a TelemetryProvider secret that does not exist")
	}
	if !strings.Contains(err.Error(), "byoc-metrics-auth") {
		t.Errorf("error %q does not name the secret", err)
	}
	if !strings.Contains(err.Error(), "TelemetryProvider") {
		t.Errorf("error %q does not say what references it", err)
	}
}

// TestTelemetryProviderSecretsReadsReferencesNotValues: the accessor walks CR
// bodies for {name, key} references, so it works for any provider the bundle
// ships without a table of them here — and it must never pick up a literal.
func TestTelemetryProviderSecretsReadsReferencesNotValues(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	if got := spec.TelemetryProviderSecrets(); len(got) != 1 || got[0] != "byoc-metrics-auth" {
		t.Fatalf("TelemetryProviderSecrets() = %v, want [byoc-metrics-auth]", got)
	}
	// A spec with no telemetry declares no requirement — which is the state a
	// foundation comes up in, before the install hint adds the export.
	if got := loadExample(t, "foundation-cli.yaml").TelemetryProviderSecrets(); len(got) != 0 {
		t.Errorf("foundation-cli.yaml requires %v; it declares no TelemetryProvider and should require none", got)
	}
}
