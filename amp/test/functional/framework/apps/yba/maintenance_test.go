package yba

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// universeWithGflags carries fields this package does not model, so a round-trip that drops them is visible.
func universeWithGflags(masterGflags, tserverGflags any) map[string]any {
	return map[string]any{
		"universeUUID": "u1",
		"name":         "uni",
		"universeDetails": map[string]any{
			"nodePrefix": "yb-admin-uni",
			"clusters": []any{
				map[string]any{
					"uuid":        "c1",
					"clusterType": "PRIMARY",
					"placementInfo": map[string]any{
						"cloudList": []any{map[string]any{"code": "gcp"}},
					},
					"userIntent": map[string]any{
						"universeName":      "uni",
						"ybSoftwareVersion": "2026.1.1.0-b40",
						"numNodes":          float64(3),
						"masterGFlags":      masterGflags,
						"tserverGFlags":     tserverGflags,
					},
				},
			},
		},
	}
}

// capture records the body of the upgrade POST for assertions.
type capture struct{ body map[string]any }

func (c *capture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &c.body); err != nil {
			t.Errorf("upgrade payload is not JSON: %v", err)
		}
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-1"})
	}
}

func (c *capture) cluster(t *testing.T) map[string]any {
	t.Helper()
	cls, ok := c.body["clusters"].([]any)
	if !ok || len(cls) == 0 {
		t.Fatalf("payload carries no clusters: %v", c.body)
	}
	m, ok := cls[0].(map[string]any)
	if !ok {
		t.Fatalf("cluster is not an object: %v", cls[0])
	}
	return m
}

func (c *capture) userIntent(t *testing.T) map[string]any {
	t.Helper()
	ui, ok := c.cluster(t)["userIntent"].(map[string]any)
	if !ok {
		t.Fatalf("cluster carries no userIntent: %v", c.cluster(t))
	}
	return ui
}

// gflagValue reads a name/value entry out of YBA's list form.
func gflagValue(t *testing.T, v any, name string) (string, bool) {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("gflags are not a list: %#v", v)
	}
	for _, item := range list {
		m, _ := item.(map[string]any)
		if m["name"] == name {
			s, _ := m["value"].(string)
			return s, true
		}
	}
	return "", false
}

func TestUpgradeGflagsMergesOntoTheUniversesOwnFlags(t *testing.T) {
	f := newFake()
	defer f.close()
	cap := &capture{}
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(
			map[string]any{"master_existing": "1"},
			map[string]any{"tserver_existing": "2"}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/gflags", cap.handler(t))

	_, err := f.client().UpgradeGflags(context.Background(), "u1",
		nil, map[string]string{"log_min_seconds_to_retain": "1800"}, UpgradeRolling, time.Second)
	if err != nil {
		t.Fatalf("UpgradeGflags: %v", err)
	}

	ui := cap.userIntent(t)
	if got, ok := gflagValue(t, ui["tserverGFlags"], "log_min_seconds_to_retain"); !ok || got != "1800" {
		t.Errorf("tserver gflag = %q (present=%v), want 1800", got, ok)
	}
	// A partial set would silently un-set every flag the universe was created with.
	if _, ok := gflagValue(t, ui["tserverGFlags"], "tserver_existing"); !ok {
		t.Error("the universe's existing tserver gflag was dropped by the upgrade")
	}
	if _, ok := gflagValue(t, ui["masterGFlags"], "master_existing"); !ok {
		t.Error("the universe's existing master gflag was dropped by the upgrade")
	}
}

// YBA has stored gflags as both a map and a name/value list across versions.
func TestUpgradeGflagsReadsTheListForm(t *testing.T) {
	f := newFake()
	defer f.close()
	cap := &capture{}
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(
			[]any{},
			[]any{map[string]any{"name": "tserver_existing", "value": "2"}}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/gflags", cap.handler(t))

	if _, err := f.client().UpgradeGflags(context.Background(), "u1",
		nil, map[string]string{"ysql_log_statement": "all"}, UpgradeRolling, time.Second); err != nil {
		t.Fatalf("UpgradeGflags: %v", err)
	}
	ui := cap.userIntent(t)
	if _, ok := gflagValue(t, ui["tserverGFlags"], "tserver_existing"); !ok {
		t.Error("a list-form existing gflag was dropped")
	}
	if got, _ := gflagValue(t, ui["tserverGFlags"], "ysql_log_statement"); got != "all" {
		t.Errorf("new gflag = %q, want all", got)
	}
}

// YBA validates the whole cluster against the live universe, so unmodelled fields must survive the round trip.
func TestUpgradePreservesUnmodelledClusterFields(t *testing.T) {
	f := newFake()
	defer f.close()
	cap := &capture{}
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/gflags", cap.handler(t))

	if _, err := f.client().UpgradeGflags(context.Background(), "u1",
		nil, map[string]string{"x": "1"}, UpgradeRolling, time.Second); err != nil {
		t.Fatalf("UpgradeGflags: %v", err)
	}
	cl := cap.cluster(t)
	if cl["placementInfo"] == nil || cl["clusterType"] != "PRIMARY" || cl["uuid"] != "c1" {
		t.Errorf("cluster lost fields the client does not model: %v", cl)
	}
	if ui := cap.userIntent(t); ui["numNodes"] != float64(3) {
		t.Errorf("userIntent lost numNodes: %v", ui)
	}
	if cap.body["nodePrefix"] != "yb-admin-uni" {
		t.Errorf("nodePrefix = %v, want the universe's own", cap.body["nodePrefix"])
	}
}

func TestUpgradeGflagsSendsTheRollingOption(t *testing.T) {
	f := newFake()
	defer f.close()
	cap := &capture{}
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/gflags", cap.handler(t))

	if _, err := f.client().UpgradeGflags(context.Background(), "u1",
		nil, map[string]string{"x": "1"}, UpgradeRolling, 90*time.Second); err != nil {
		t.Fatalf("UpgradeGflags: %v", err)
	}
	if cap.body["upgradeOption"] != "Rolling" {
		t.Errorf("upgradeOption = %v, want Rolling (the only option that stays serving)", cap.body["upgradeOption"])
	}
	if cap.body["taskType"] != "GFlags" {
		t.Errorf("taskType = %v, want GFlags", cap.body["taskType"])
	}
	if cap.body["sleepAfterTServerRestartMillis"] != float64(90000) {
		t.Errorf("tserver restart sleep = %v, want 90000ms", cap.body["sleepAfterTServerRestartMillis"])
	}
	if cap.body["ybSoftwareVersion"] != "2026.1.1.0-b40" {
		t.Errorf("ybSoftwareVersion = %v, want the universe's current version", cap.body["ybSoftwareVersion"])
	}
}

func TestUpgradeGflagsRejectsAnEmptyChange(t *testing.T) {
	f := newFake()
	defer f.close()
	if _, err := f.client().UpgradeGflags(context.Background(), "u1", nil, nil, UpgradeRolling, time.Second); err == nil {
		t.Fatal("an upgrade with no gflags must be rejected before it reaches YBA")
	}
}

func TestUpgradeSoftwareSendsTheTargetVersion(t *testing.T) {
	f := newFake()
	defer f.close()
	cap := &capture{}
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/db_version", cap.handler(t))

	if _, err := f.client().UpgradeSoftware(context.Background(), "u1", "2026.1.2.0-b67", UpgradeRolling, time.Second); err != nil {
		t.Fatalf("UpgradeSoftware: %v", err)
	}
	if cap.body["ybSoftwareVersion"] != "2026.1.2.0-b67" {
		t.Errorf("ybSoftwareVersion = %v, want the TARGET version", cap.body["ybSoftwareVersion"])
	}
	if cap.body["taskType"] != "Software" {
		t.Errorf("taskType = %v, want Software", cap.body["taskType"])
	}
}

func TestUpgradeSoftwareRejectsAnEmptyVersion(t *testing.T) {
	f := newFake()
	defer f.close()
	if _, err := f.client().UpgradeSoftware(context.Background(), "u1", "", UpgradeRolling, time.Second); err == nil {
		t.Fatal("an upgrade to no version must be rejected")
	}
}

// Stopping at "task succeeded" leaves the new version's auto-flags inert; the finalize must follow.
func TestUpgradeSoftwareAndWaitFinalizesWhenRequired(t *testing.T) {
	f := newFake()
	defer f.close()
	finalized := false
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/db_version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-up", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskStatus{Status: "Success", Percent: 100})
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/software/precheck", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"finalizeRequired": true})
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/finalize", func(w http.ResponseWriter, _ *http.Request) {
		finalized = true
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-fin"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-fin", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskStatus{Status: "Success", Percent: 100})
	})

	if err := f.client().UpgradeSoftwareAndWait(context.Background(), "u1", "2026.1.2.0-b67", UpgradeRolling, time.Second); err != nil {
		t.Fatalf("UpgradeSoftwareAndWait: %v", err)
	}
	if !finalized {
		t.Fatal("the upgrade was never finalized despite the precheck demanding it")
	}
}

func TestUpgradeSoftwareAndWaitSkipsAnUnneededFinalize(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/db_version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-up", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskStatus{Status: "Success", Percent: 100})
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/software/precheck", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"finalizeRequired": false})
	})
	if err := f.client().UpgradeSoftwareAndWait(context.Background(), "u1", "2026.1.2.0-b67", UpgradeRolling, time.Second); err != nil {
		t.Fatalf("UpgradeSoftwareAndWait: %v", err)
	}
}

func TestUpgradeSoftwareAndWaitSurfacesAFailedTask(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/db_version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-up", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskStatus{Status: "Failure", Title: "Rolling upgrade"})
	})
	if err := f.client().UpgradeSoftwareAndWait(context.Background(), "u1", "2026.1.2.0-b67", UpgradeRolling, time.Second); err == nil {
		t.Fatal("a failed upgrade task must fail the call")
	}
}

func TestUpgradeSoftwareAndWaitRetriesAFailedKubectlCall(t *testing.T) {
	defer func(d time.Duration) { KubectlRetryDelay = d }(KubectlRetryDelay)
	KubectlRetryDelay = time.Millisecond
	for _, tc := range []struct {
		name      string
		failure   string
		retryWins bool
	}{
		{"a ybc upload whose retry succeeds", "hit error: Could not upload the ybc contents.", true},
		{"an exec whose retry succeeds", "Unable to run the command: uname -m. Code: 1. Output: Error from server: error dialing backend: dial timeout, backstop", true},
		{"a ybc upload every retry fails", "hit error: Could not upload the ybc contents.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			defer f.close()
			retries := 0
			f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
			})
			f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/db_version", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
			})
			f.on("GET /api/v1/customers/cust1/tasks/task-up", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, TaskStatus{Status: "Failure", Title: "Upgraded Software"})
			})
			f.on("GET /api/v1/customers/cust1/tasks/task-up/failed", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"failedSubTasks": []map[string]any{{"errorString": tc.failure}}})
			})
			f.on("POST /api/v1/customers/cust1/tasks/task-up/retry", func(w http.ResponseWriter, _ *http.Request) {
				retries++
				next := "task-up"
				if tc.retryWins {
					next = "task-ok"
				}
				writeJSON(w, http.StatusOK, TaskRef{TaskUUID: next})
			})
			f.on("GET /api/v1/customers/cust1/tasks/task-ok", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, TaskStatus{Status: "Success", Percent: 100})
			})
			f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/software/precheck", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"finalizeRequired": false})
			})

			err := f.client().UpgradeSoftwareAndWait(context.Background(), "u1", "2026.1.2.0-b67", UpgradeRolling, time.Second)
			if tc.retryWins {
				if err != nil || retries != 1 {
					t.Fatalf("err = %v after %d retries, want success after 1", err, retries)
				}
				return
			}
			if err == nil || !core.IsEnvFailure(err) || retries != KubectlRetries {
				t.Fatalf("err = %v (env %v) after %d retries, want an env failure after %d", err, core.IsEnvFailure(err), retries, KubectlRetries)
			}
		})
	}
}

func TestRestartUniversePostsTheRestartEndpoint(t *testing.T) {
	f := newFake()
	defer f.close()
	cap := &capture{}
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/restart", cap.handler(t))

	if _, err := f.client().RestartUniverse(context.Background(), "u1", UpgradeRolling, time.Second); err != nil {
		t.Fatalf("RestartUniverse: %v", err)
	}
	if cap.body["upgradeOption"] != "Rolling" {
		t.Errorf("upgradeOption = %v, want Rolling", cap.body["upgradeOption"])
	}
}

func TestUpgradeErrorsWhenTheUniverseHasNoClusters(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"universeUUID": "u1", "universeDetails": map[string]any{}})
	})
	if _, err := f.client().RestartUniverse(context.Background(), "u1", UpgradeRolling, time.Second); err == nil {
		t.Fatal("a universe with no clusters must error rather than post an empty payload")
	}
}

func TestPreviousReleaseTrain(t *testing.T) {
	cases := map[string]string{
		"2026.1.0.0-b1":  "2025.2",
		"2026.2.3.0-b17": "2026.1",
		"2025.1.0.0":     "2024.2",
		"2024.2.1.0-b5":  "2024.1",
	}
	for in, want := range cases {
		if got := PreviousReleaseTrain(in); got != want {
			t.Errorf("PreviousReleaseTrain(%q) = %q, want %q", in, got, want)
		}
	}
}

// A version present in the catalog needs no refresh and no import.
func TestEnsureReleaseAvailableIsANoOpWhenPresent(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []Release{{Version: "2026.1.0.0-b1", Artifacts: []ReleaseArtifact{{Architecture: "x86_64"}}}})
	})
	if err := f.client().EnsureReleaseAvailable(context.Background(), "2026.1.0.0-b1", "x86_64", false); err != nil {
		t.Fatalf("EnsureReleaseAvailable: %v", err)
	}
}

// A refresh is tried before an import: it is cheap and usually enough.
func TestEnsureReleaseAvailableRefreshesBeforeImporting(t *testing.T) {
	f := newFake()
	defer f.close()
	refreshed := false
	calls := 0
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if refreshed {
			writeJSON(w, http.StatusOK, []Release{{Version: "2026.1.0.0-b1", Artifacts: []ReleaseArtifact{{Platform: "LINUX", Architecture: "x86_64"}}}})
			return
		}
		writeJSON(w, http.StatusOK, []Release{})
	})
	f.on("PUT /api/v1/customers/cust1/releases", func(w http.ResponseWriter, _ *http.Request) {
		refreshed = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	if err := f.client().EnsureReleaseAvailable(context.Background(), "2026.1.0.0-b1", "x86_64", false); err != nil {
		t.Fatalf("EnsureReleaseAvailable: %v", err)
	}
	if !refreshed {
		t.Error("the catalog was never refreshed")
	}
	if calls < 2 {
		t.Errorf("catalog read %d times, want a re-check after the refresh", calls)
	}
}

// Still absent after a refresh: import it, rather than let the upgrade fail on YBA's vaguer error.
func TestEnsureReleaseAvailableImportsWhenStillAbsent(t *testing.T) {
	f := newFake()
	defer f.close()
	imported := false
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		if imported {
			writeJSON(w, http.StatusOK, []Release{{Version: "2026.1.0.0-b1", Artifacts: []ReleaseArtifact{{Architecture: "x86_64"}}}})
			return
		}
		writeJSON(w, http.StatusOK, []Release{})
	})
	f.on("PUT /api/v1/customers/cust1/releases", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	f.on("POST /api/v1/customers/cust1/ybdb_release/extract_metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractHandle{ResourceUUID: "m1"})
	})
	f.on("GET /api/v1/customers/cust1/ybdb_release/extract_metadata/m1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractedArtifact{
			Version: "2026.1.0.0-b1", Platform: "LINUX", Architecture: "x86_64",
			Sha256: "abc", Status: urlArtifactSuccess,
		})
	})
	f.on("POST /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		imported = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	if err := f.client().EnsureReleaseAvailable(context.Background(), "2026.1.0.0-b1", "x86_64", false); err != nil {
		t.Fatalf("EnsureReleaseAvailable: %v", err)
	}
	if !imported {
		t.Fatal("the missing release was never imported")
	}
}

func TestEnsureReleaseAvailableRejectsAnEmptyVersion(t *testing.T) {
	f := newFake()
	defer f.close()
	if err := f.client().EnsureReleaseAvailable(context.Background(), "", "x86_64", false); err == nil {
		t.Fatal("an empty version must be rejected")
	}
}

func TestHelmChartURLUsesTheShortChartName(t *testing.T) {
	got := HelmChartURL("2025.2.7.0-b7")
	want := "https://releases.yugabyte.com/2025.2.7.0-b7/helm/yugabyte-2025.2.7.tgz"
	if got != want {
		t.Errorf("HelmChartURL = %q, want %q", got, want)
	}
}

// A k8s universe needs the helm chart artifact; the VM artifact alone must not satisfy the check.
func TestEnsureReleaseAvailableK8sDemandsTheChartArtifact(t *testing.T) {
	f := newFake()
	defer f.close()
	imported := false
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		arts := []ReleaseArtifact{{Platform: "LINUX", Architecture: "x86_64", PackageURL: "https://x/el8.tar.gz"}}
		if imported {
			arts = append(arts, ReleaseArtifact{Platform: "KUBERNETES"})
		}
		writeJSON(w, http.StatusOK, []Release{{ReleaseUUID: "r1", Version: "2025.2.7.0-b7", Artifacts: arts}})
	})
	f.on("PUT /api/v1/customers/cust1/releases", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	f.on("POST /api/v1/customers/cust1/ybdb_release/extract_metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractHandle{ResourceUUID: "m1"})
	})
	f.on("GET /api/v1/customers/cust1/ybdb_release/extract_metadata/m1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractedArtifact{Version: "2025.2.7.0-b7", Platform: "KUBERNETES", Sha256: "abc", Status: urlArtifactSuccess})
	})
	f.on("PUT /api/v1/customers/cust1/ybdb_release/r1", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		arts, _ := body["artifacts"].([]any)
		if len(arts) != 2 {
			t.Errorf("artifacts = %v, want the existing one preserved plus the chart", arts)
		}
		imported = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	if err := f.client().EnsureReleaseAvailable(context.Background(), "2025.2.7.0-b7", "x86_64", true); err != nil {
		t.Fatalf("EnsureReleaseAvailable(k8s): %v", err)
	}
	if !imported {
		t.Fatal("the chart artifact was never imported despite the VM artifact being present")
	}
}

func TestEnsureReleaseK8sIsANoOpWhenTheChartIsPresent(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []Release{{Version: "2025.2.7.0-b7", Artifacts: []ReleaseArtifact{{Platform: "KUBERNETES"}}}})
	})
	if err := f.client().EnsureReleaseK8s(context.Background(), "2025.2.7.0-b7"); err != nil {
		t.Fatalf("EnsureReleaseK8s: %v", err)
	}
}

// YBA 500s deriving DB metadata from a chart alone, so the release is created from the tarball first, then the chart is attached.
func TestEnsureReleaseK8sCreatesFromTheTarballFirst(t *testing.T) {
	f := newFake()
	defer f.close()
	var tarballCreated, chartAdded bool
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		switch {
		case chartAdded:
			writeJSON(w, http.StatusOK, []Release{{ReleaseUUID: "r1", Version: "2025.2.7.0-b7",
				Artifacts: []ReleaseArtifact{{Platform: "LINUX", Architecture: "x86_64"}, {Platform: "KUBERNETES"}}}})
		case tarballCreated:
			writeJSON(w, http.StatusOK, []Release{{ReleaseUUID: "r1", Version: "2025.2.7.0-b7",
				Artifacts: []ReleaseArtifact{{Platform: "LINUX", Architecture: "x86_64"}}}})
		default:
			writeJSON(w, http.StatusOK, []Release{})
		}
	})
	f.on("POST /api/v1/customers/cust1/ybdb_release/extract_metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractHandle{ResourceUUID: "m1"})
	})
	f.on("GET /api/v1/customers/cust1/ybdb_release/extract_metadata/m1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractedArtifact{Version: "2025.2.7.0-b7", Sha256: "abc", Status: urlArtifactSuccess})
	})
	f.on("POST /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		tarballCreated = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	f.on("PUT /api/v1/customers/cust1/ybdb_release/r1", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !tarballCreated {
			t.Error("the chart was attached before the release existed")
		}
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		if arts, _ := body["artifacts"].([]any); len(arts) != 2 {
			t.Errorf("artifacts = %v, want the tarball preserved plus the chart", arts)
		}
		chartAdded = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	if err := f.client().EnsureReleaseK8s(context.Background(), "2025.2.7.0-b7"); err != nil {
		t.Fatalf("EnsureReleaseK8s: %v", err)
	}
	if !tarballCreated || !chartAdded {
		t.Fatalf("tarballCreated=%v chartAdded=%v, want both", tarballCreated, chartAdded)
	}
}

// An imported build differs from the YBA's own, so the version gates are relaxed before validation, or a stable/preview crossing or downgrade gets refused outright.
func TestEnsureReleaseK8sRelaxesTheVersionGates(t *testing.T) {
	f := newFake()
	defer f.close()
	var keys []string
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []Release{{ReleaseUUID: "r1", Version: "2025.2.7.0-b7",
			Artifacts: []ReleaseArtifact{{Platform: "LINUX", Architecture: "x86_64"}}}})
	})
	for _, k := range []string{"yb.skip_version_checks", "yb.allow_db_version_more_than_yba_version"} {
		key := k
		f.on("PUT /api/v1/customers/cust1/runtime_config/"+GlobalRuntimeScope+"/key/"+key, func(w http.ResponseWriter, _ *http.Request) {
			keys = append(keys, key)
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
		})
	}
	f.on("POST /api/v1/customers/cust1/ybdb_release/extract_metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractHandle{ResourceUUID: "m1"})
	})
	f.on("GET /api/v1/customers/cust1/ybdb_release/extract_metadata/m1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractedArtifact{Version: "2025.2.7.0-b7", Platform: "KUBERNETES", Sha256: "abc", Status: urlArtifactSuccess})
	})
	f.on("PUT /api/v1/customers/cust1/ybdb_release/r1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	// The chart never lands in the fake's catalog, so the wait times out; the gates must already be set by then.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = f.client().EnsureReleaseK8s(ctx, "2025.2.7.0-b7")
	if len(keys) != 2 {
		t.Errorf("runtime keys set = %v, want both version gates relaxed", keys)
	}
}

// A YBA that rejects the runtime key must not fail the import.
func TestAllowCrossVersionReleasesToleratesAnUnknownKey(t *testing.T) {
	f := newFake()
	defer f.close()
	f.client().AllowCrossVersionReleases(context.Background())
}

// A DB newer than the YBA is refused at CREATE ("Release version X is newer then
// yba version Y"), so the gates must be down before the POST, not after it.
func TestEnsureReleaseRelaxesTheVersionGatesBeforeCreating(t *testing.T) {
	f := newFake()
	defer f.close()
	var gates int
	var created bool
	f.on("GET /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		if created {
			writeJSON(w, http.StatusOK, []Release{{ReleaseUUID: "r1", Version: "2026.1.2.0-b77",
				Artifacts: []ReleaseArtifact{{Platform: "LINUX", Architecture: "x86_64"}}}})
			return
		}
		writeJSON(w, http.StatusOK, []Release{})
	})
	for _, k := range []string{"yb.skip_version_checks", "yb.allow_db_version_more_than_yba_version"} {
		f.on("PUT /api/v1/customers/cust1/runtime_config/"+GlobalRuntimeScope+"/key/"+k, func(w http.ResponseWriter, _ *http.Request) {
			gates++
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
		})
	}
	f.on("POST /api/v1/customers/cust1/ybdb_release/extract_metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractHandle{ResourceUUID: "m1"})
	})
	f.on("GET /api/v1/customers/cust1/ybdb_release/extract_metadata/m1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, extractedArtifact{Version: "2026.1.2.0-b77", Sha256: "abc", Status: urlArtifactSuccess})
	})
	f.on("POST /api/v1/customers/cust1/ybdb_release", func(w http.ResponseWriter, _ *http.Request) {
		if gates != 2 {
			t.Errorf("gates relaxed = %d at create time, want both", gates)
		}
		created = true
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
	})
	if err := f.client().EnsureRelease(context.Background(), "2026.1.2.0-b77", "x86_64",
		ReleaseTarballURL("2026.1.2.0-b77", "x86_64")); err != nil {
		t.Fatalf("EnsureRelease: %v", err)
	}
	if !created {
		t.Fatal("the release was never created")
	}
}

func TestRestartUniverseAndWaitDoesNotRetryAPollError(t *testing.T) {
	defer func(d time.Duration) { KubectlRetryDelay = d }(KubectlRetryDelay)
	KubectlRetryDelay = time.Millisecond
	f := newFake()
	defer f.close()
	retries := 0
	f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
	})
	f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/restart", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
	})
	f.on("GET /api/v1/customers/cust1/tasks/task-up", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "error dialing backend: dial timeout", http.StatusBadRequest)
	})
	f.on("POST /api/v1/customers/cust1/tasks/task-up/retry", func(w http.ResponseWriter, _ *http.Request) {
		retries++
		writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
	})
	err := f.client().RestartUniverseAndWait(context.Background(), "u1", UpgradeRolling, time.Second)
	if err == nil || retries != 0 {
		t.Fatalf("err = %v after %d retries, want the poll error returned unretried", err, retries)
	}
}

func TestRestartUniverseAndWaitAdoptsTheLockAfterARetryPostErrors(t *testing.T) {
	defer func(d, l, p time.Duration) { KubectlRetryDelay, retryLockTimeout, retryLockPoll = d, l, p }(KubectlRetryDelay, retryLockTimeout, retryLockPoll)
	KubectlRetryDelay, retryLockTimeout, retryLockPoll = time.Millisecond, 100*time.Millisecond, time.Millisecond
	for _, tc := range []struct {
		name   string
		lockBy string
	}{
		{"the retry landed", "task-ok"},
		{"the retry never landed", "task-up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			defer f.close()
			locked := func() map[string]any {
				u := universeWithGflags(map[string]any{}, map[string]any{})
				d := u["universeDetails"].(map[string]any)
				d["updateInProgress"] = tc.lockBy == "task-ok"
				d["updatingTaskUUID"] = tc.lockBy
				return u
			}
			f.on("GET /api/v1/customers/cust1/universes/u1", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, universeWithGflags(map[string]any{}, map[string]any{}))
			})
			f.on("GET /api/v1/customers/cust1/universes", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, []any{locked()})
			})
			f.on("POST /api/v1/customers/cust1/universes/u1/upgrade/restart", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, TaskRef{TaskUUID: "task-up"})
			})
			f.on("GET /api/v1/customers/cust1/tasks/task-up", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, TaskStatus{Status: "Failure", Title: "Restart Universe"})
			})
			f.on("GET /api/v1/customers/cust1/tasks/task-up/failed", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"failedSubTasks": []map[string]any{{"errorString": "hit error: Could not upload the ybc contents."}}})
			})
			f.on("POST /api/v1/customers/cust1/tasks/task-up/retry", func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "connection reset", http.StatusBadRequest)
			})
			f.on("GET /api/v1/customers/cust1/tasks/task-ok", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, TaskStatus{Status: "Success", Percent: 100})
			})
			err := f.client().RestartUniverseAndWait(context.Background(), "u1", UpgradeRolling, time.Second)
			if tc.lockBy == "task-ok" {
				if err != nil {
					t.Fatalf("err = %v, want the adopted retry waited to success", err)
				}
				return
			}
			if err == nil || !core.IsEnvFailure(err) {
				t.Fatalf("err = %v (env %v), want an env failure", err, core.IsEnvFailure(err))
			}
		})
	}
}
