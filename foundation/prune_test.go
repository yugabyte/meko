package foundation

import (
	"context"

	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ownedList is what `kubectl get <kinds> -l managed-by=... -o json` returns
// for a namespace holding these objects.
func ownedList(objects ...string) string {
	var items []string
	for _, o := range objects {
		kind, name, _ := strings.Cut(o, "/")
		items = append(items, fmt.Sprintf(
			`{"apiVersion":"operator.yugabyte.io/v1alpha1","kind":%q,"metadata":{"name":%q}}`, kind, name))
	}
	return `{"items":[` + strings.Join(items, ",") + `]}`
}

// pruneDeployer is a Deployer whose cluster reports `objects` as this
// foundation's, with discovery answering normally.
func pruneDeployer(t *testing.T, spec *Spec, extra []Response, objects ...string) (*Deployer, *Recorder) {
	t.Helper()
	// Prune's own stubs first: the labelled query's argv contains "get
	// ybuniverses,…", which a looser caller-supplied "get ybuniverse" match
	// would otherwise swallow.
	rec := &Recorder{Responses: append([]Response{
		{Match: "api-resources", Result: Result{Stdout: "ybuniverses\nybproviders\nstorageconfigs\nkmsconfigs\nreleases\n"}},
		{Match: "-l " + ManagedByLabel, Result: Result{Stdout: ownedList(objects...)}},
		{Match: "get -f -", Result: Result{ExitCode: 1, Stderr: "NotFound"}},
	}, extra...)}
	d := New(spec, rec.Runner()).
		WithTimeouts(time.Second, time.Second, time.Millisecond).
		WithLogger(func(string, ...any) {})
	return d, rec
}

func rendered(t *testing.T, spec *Spec) []Manifest {
	t.Helper()
	m, err := spec.Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return m
}

// TestPruneDeletesWhatTheSpecDropped is the point of pruning: without it,
// removing a key stops sending an object without removing it, and the spec
// stops being the source of truth.
func TestPruneDeletesWhatTheSpecDropped(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil,
		"YBProvider/foundation", "YBUniverse/foundation", "KMSConfig/gone", "StorageConfig/also-gone")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	want := []string{"StorageConfig/also-gone", "KMSConfig/gone"}
	if got := deletedObjects(t, rec); !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

// TestPruneLeavesUnlabelledObjectsAlone is the test that stops this becoming a
// namespace wiper. The delete set is drawn from a LABELLED query, so an object
// somebody else created is never in it — the namespace is shared with the
// consumer's own workloads.
func TestPruneLeavesUnlabelledObjectsAlone(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil, "YBProvider/foundation", "YBUniverse/foundation")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := deletedObjects(t, rec); len(got) != 0 {
		t.Fatalf("prune deleted %v with nothing stale", got)
	}
	var labelled bool
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "get ") && strings.Contains(a, ManagedByLabel+"="+ManagedByValue) {
			labelled = true
		}
	}
	if !labelled {
		t.Fatalf("the candidate query is not label-scoped, so it would see objects we did not "+
			"create; argv: %v", rec.Argvs())
	}
}

// TestPruneIsANoOpWhenNothingWasDropped: a prune that always finds something
// to delete is the scariest bug shape there is, so the quiet case is pinned
// too.
func TestPruneIsANoOpWhenNothingWasDropped(t *testing.T) {
	spec := loadExample(t, "resources.yaml")
	var owned []string
	for _, m := range rendered(t, spec) {
		owned = append(owned, m.String())
	}
	d, rec := pruneDeployer(t, spec, nil, owned...)
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := deletedObjects(t, rec); len(got) != 0 {
		t.Fatalf("prune deleted %v when the cluster already matched the spec", got)
	}
}

// TestPruneNeverTouchesTheProviderOrUniverse: both are always rendered, so
// they are always in the keep set. This is the guard against a future Render
// change quietly making the universe a prune candidate.
func TestPruneNeverTouchesTheProviderOrUniverse(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil, "YBProvider/foundation", "YBUniverse/foundation", "Release/stale")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, got := range deletedObjects(t, rec) {
		if strings.HasPrefix(got, "YBUniverse/") || strings.HasPrefix(got, "YBProvider/") {
			t.Fatalf("prune deleted %s, which the spec declares", got)
		}
	}
}

// TestPruneFailsRatherThanAssumingNothingWhenDiscoveryFails: "I could not list
// anything" and "there is nothing to delete" must not look the same to a
// delete loop. The empty case is the dangerous one — it reads as success and
// silently stops pruning forever.
func TestPruneFailsRatherThanAssumingNothingWhenDiscoveryFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result Result
		want   string
	}{
		{"discovery errors", Result{ExitCode: 1, Stderr: "the server is currently unable"}, "list the operator's kinds"},
		{"discovery is empty", Result{Stdout: "\n"}, "refusing to decide what to delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := loadExample(t, "amp.yaml")
			rec := &Recorder{Responses: []Response{
				{Match: "api-resources", Result: tc.result},
			}}
			d := New(spec, rec.Runner()).
				WithTimeouts(time.Second, time.Second, time.Millisecond).
				WithLogger(func(string, ...any) {})
			err := d.prune(context.Background(), rendered(t, spec))
			if err == nil {
				t.Fatal("prune carried on without knowing what exists")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			if got := deletedObjects(t, rec); len(got) != 0 {
				t.Errorf("prune deleted %v despite not knowing what exists", got)
			}
		})
	}
}

// TestPruneDiscoversKindsFromTheCluster: the kinds come from the cluster, not
// from a list in Go. That is what lets a kind shipped in a future chart be
// pruned with no code change, and what means there is nothing here to keep in
// step with the bundle.
func TestPruneDiscoversKindsFromTheCluster(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil, "YBProvider/foundation", "YBUniverse/foundation")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var asked bool
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "api-resources") && strings.Contains(a, "--api-group="+operatorAPIGroup) {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("prune did not ask the cluster which kinds exist; argv: %v", rec.Argvs())
	}
}

// TestPruneIsNamespaceScoped: one foundation must not reach another's objects,
// and two share a cluster routinely.
func TestPruneIsNamespaceScoped(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil, "YBProvider/foundation", "YBUniverse/foundation", "Release/stale")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, c := range rec.Calls {
		joined := strings.Join(c.Argv, " ")
		if !strings.Contains(joined, ManagedByLabel) {
			continue
		}
		if !strings.Contains(joined, "-n "+spec.Namespace) {
			t.Fatalf("the candidate query is not namespace-scoped: %q", joined)
		}
	}
}

// TestPruneRunsAfterTheUniverseSettles is the ordering the whole design rests
// on. One edit can both stop the universe using a KMSConfig and drop the key;
// disabling encryption at rest is a YBA TASK, so a prune that ran before the
// universe settled would delete a config still referenced by a running edit.
func TestPruneRunsAfterTheUniverseSettles(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, []Response{
		{Match: "get ybprovider", Result: Result{Stdout: `{"status":{"state":"Ready"}}`}},
		{Match: "get ybcertificate", Result: Result{Stdout: `{"status":{"success":true,"resourceUUID":"c1"}}`}},
		// Still editing on the first poll, settled on the second.
		{Match: "get ybuniverse", Once: true, Result: Result{Stdout: statusWithAction(
			"Editing", "EditUniverse", "running", "disabling encryption at rest")}},
		{Match: "get ybuniverse", Result: Result{Stdout: readyStatus()}},
	}, "YBProvider/foundation", "YBUniverse/foundation", "KMSConfig/gone")

	if err := d.applyCRs(context.Background()); err != nil {
		t.Fatalf("applyCRs: %v", err)
	}
	if err := d.waitUniverse(context.Background()); err != nil {
		t.Fatalf("waitUniverse: %v", err)
	}
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	argvs := rec.Argvs()
	lastSettledPoll, firstDelete := -1, -1
	for i, a := range argvs {
		if strings.Contains(a, "get ybuniverse") {
			lastSettledPoll = i
		}
		if strings.Contains(a, "delete -f -") && firstDelete < 0 {
			firstDelete = i
		}
	}
	if firstDelete < 0 {
		t.Fatalf("nothing was pruned; argv: %v", argvs)
	}
	if firstDelete < lastSettledPoll {
		t.Errorf("prune deleted before the universe settled; argv: %v", argvs)
	}
}

// TestNoPruneSkipsTheStep covers the escape hatch, and that it is off by
// default: the default has to be that the spec is the source of truth.
func TestNoPruneSkipsTheStep(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil, "YBProvider/foundation", "YBUniverse/foundation", "Release/stale")
	d = d.WithoutPrune()
	if d.noPrune != true {
		t.Fatal("WithoutPrune did not take")
	}
	// converge is what honours the flag, so assert on the field and then that
	// prune itself still works when called — the two are separate concerns.
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := deletedObjects(t, rec); len(got) != 1 {
		t.Fatalf("deleted %v, want the one stale object when prune is called directly", got)
	}
	if New(spec, rec.Runner()).noPrune {
		t.Error("pruning is off by default; the spec would stop being the source of truth")
	}
}

// TestPruneUsesDeleteCRSoFinalizersAreHandled: a pruned CR carries the same
// operator finalizer as one removed by `down`, so it needs the same
// delete-then-poll-for-absence treatment rather than a bare delete.
func TestPruneUsesDeleteCRSoFinalizersAreHandled(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil, "YBProvider/foundation", "YBUniverse/foundation", "Release/stale")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var sawWaitFalse, sawPoll bool
	for _, a := range rec.Argvs() {
		if strings.Contains(a, "delete -f -") && strings.Contains(a, "--wait=false") {
			sawWaitFalse = true
		}
		if strings.Contains(a, "get -f -") {
			sawPoll = true
		}
	}
	if !sawWaitFalse || !sawPoll {
		t.Errorf("prune did not go through deleteCR; argv: %v", rec.Argvs())
	}
}

// TestPruneIssuesEveryDeleteBeforeWaiting: "no longer in the spec" does not
// mean "references nothing" — two entries dropped in the same edit can
// reference each other, a StorageConfig and the BackupSchedule using it being
// the obvious pair, and their Kind/name order bears no relation to that.
//
// Deleting one at a time would block on the live reference for the whole
// timeout and fail the converge. Issuing every delete first lets the operator
// settle them in whatever order it needs.
func TestPruneIssuesEveryDeleteBeforeWaiting(t *testing.T) {
	spec := loadExample(t, "amp.yaml")
	d, rec := pruneDeployer(t, spec, nil,
		"YBProvider/foundation", "YBUniverse/foundation",
		"StorageConfig/backups", "BackupSchedule/nightly")
	if err := d.prune(context.Background(), rendered(t, spec)); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var lastDelete, firstWait = -1, -1
	for i, c := range rec.Calls {
		joined := strings.Join(c.Argv, " ")
		if strings.Contains(joined, "delete -f -") {
			lastDelete = i
		}
		if strings.Contains(joined, "get -f -") && firstWait < 0 {
			firstWait = i
		}
	}
	if lastDelete < 0 || firstWait < 0 {
		t.Fatalf("expected deletes and waits; argv: %v", rec.Argvs())
	}
	if firstWait < lastDelete {
		t.Errorf("prune waited on one object before asking for the others; argv: %v", rec.Argvs())
	}
}
