package reap

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/apps/cloud"
)

// TestPlanJSONRoundTrip: a plan survives the registry-record round trip verbatim,
// and an empty/nil plan serializes to "" (nothing recorded).
func TestPlanJSONRoundTrip(t *testing.T) {
	in := &Plan{
		Cloud: &CloudReap{Cloud: "gcp", RunTag: "deadbeef", Region: "us-west1"},
		Namespaces: []NSReap{
			{Kind: KindYBAStack, KubeContext: "gke_ctx", Namespace: "ampqa-yba-deadbeef", CRBs: []string{"crb-ampqa-yba-deadbeef"}},
			{Kind: KindDiscover, KubeContext: "gke_ctx", Namespace: "ampqa-deadbeef"},
		},
	}
	s, err := in.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	got, err := FromJSON(s)
	if err != nil {
		t.Fatalf("FromJSON: %v", err)
	}
	if !reflect.DeepEqual(in, got) {
		t.Errorf("round trip mismatch:\n in=%+v\ngot=%+v", in, got)
	}

	for _, empty := range []*Plan{nil, {}} {
		s, err := empty.JSON()
		if err != nil {
			t.Fatalf("empty JSON: %v", err)
		}
		if s != "" {
			t.Errorf("empty plan JSON = %q, want \"\"", s)
		}
	}
	if p, err := FromJSON(""); err != nil || p != nil {
		t.Errorf("FromJSON(\"\") = (%v, %v), want (nil, nil)", p, err)
	}
}

// TestExecuteInvokesRecordedCloudReap: a cloud plan reaps by the RECORDED run
// tag — the exact tag the provisioner stamped, region-wide.
func TestExecuteInvokesRecordedCloudReap(t *testing.T) {
	var gotCloud, gotTag, gotRegion string
	e := &executor{
		cloudReap: func(_ context.Context, c *CloudReap, _ bool) error {
			gotCloud, gotTag, gotRegion = c.Cloud, c.RunTag, c.Region
			return nil
		},
		nsReap: func(context.Context, NSReap) error { t.Fatal("nsReap must not run for a cloud-only plan"); return nil },
	}
	p := &Plan{Cloud: &CloudReap{Cloud: "aws", RunTag: "cafef00d", Region: "us-west-2"}}
	if err := e.run(context.Background(), p, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if gotCloud != "aws" || gotTag != "cafef00d" || gotRegion != "us-west-2" {
		t.Errorf("cloud reap called with (%q,%q,%q), want (aws,cafef00d,us-west-2)", gotCloud, gotTag, gotRegion)
	}
}

// TestExecuteInvokesEachNamespaceReap: every recorded namespace is reaped, with
// its kind + name + kubecontext passed through unchanged.
func TestExecuteInvokesEachNamespaceReap(t *testing.T) {
	var got []NSReap
	e := &executor{
		cloudReap: func(context.Context, *CloudReap, bool) error {
			t.Fatal("cloudReap must not run for a ns-only plan")
			return nil
		},
		nsReap: func(_ context.Context, n NSReap) error { got = append(got, n); return nil },
	}
	p := &Plan{Namespaces: []NSReap{
		{Kind: KindYBK8s, KubeContext: "ctx1", Namespace: "ampqa-yb-1"},
		{Kind: KindYBAStack, KubeContext: "ctx2", Namespace: "ampqa-yba-2", CRBs: []string{"crb-2"}},
		{Kind: KindDiscover, KubeContext: "ctx2", Namespace: "ampqa-2"},
	}}
	if err := e.run(context.Background(), p, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !reflect.DeepEqual(got, p.Namespaces) {
		t.Errorf("namespace reaps = %+v, want %+v", got, p.Namespaces)
	}
}

// TestExecuteSurfacesFailuresLoudly: when reap steps fail, Execute runs EVERY
// step (no early return) and returns one aggregated error naming each failure —
// a missed reap is never a silent success.
func TestExecuteSurfacesFailuresLoudly(t *testing.T) {
	nsCalls := 0
	e := &executor{
		cloudReap: func(context.Context, *CloudReap, bool) error { return errString("cloud boom") },
		nsReap:    func(context.Context, NSReap) error { nsCalls++; return errString("ns boom") },
	}
	p := &Plan{
		Cloud:      &CloudReap{Cloud: "aws", RunTag: "t"},
		Namespaces: []NSReap{{Kind: KindYBK8s, Namespace: "a"}, {Kind: KindYBK8s, Namespace: "b"}},
	}
	err := e.run(context.Background(), p, false)
	if err == nil {
		t.Fatal("run returned nil despite reap failures — a leak would be reported as success")
	}
	if nsCalls != 2 {
		t.Errorf("nsReap ran %d times, want 2 (every step runs despite earlier failure)", nsCalls)
	}
	for _, want := range []string{"leaked", "cloud boom", "ns boom"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestExecuteEmptyPlanIsNoop: an empty plan reaps nothing and errors nothing.
func TestExecuteEmptyPlanIsNoop(t *testing.T) {
	e := &executor{
		cloudReap: func(context.Context, *CloudReap, bool) error { t.Fatal("no reap for empty plan"); return nil },
		nsReap:    func(context.Context, NSReap) error { t.Fatal("no reap for empty plan"); return nil },
	}
	if err := e.run(context.Background(), &Plan{}, false); err != nil {
		t.Errorf("empty plan run = %v, want nil", err)
	}
	if err := e.run(context.Background(), nil, false); err != nil {
		t.Errorf("nil plan run = %v, want nil", err)
	}
}

// TestDefaultNSReapKindDispatch: the default namespace reap routes by kind and
// rejects an unknown kind + a discover with no universe name (guards before any
// kubectl call).
func TestDefaultNSReapKindDispatch(t *testing.T) {
	if err := defaultNSReap(context.Background(), NSReap{Kind: "bogus", Namespace: "x"}); err == nil ||
		!strings.Contains(err.Error(), "unknown namespace reap kind") {
		t.Errorf("unknown kind err = %v, want 'unknown namespace reap kind'", err)
	}
	if err := defaultNSReap(context.Background(), NSReap{Kind: KindDiscover, Namespace: ""}); err == nil ||
		!strings.Contains(err.Error(), "empty universe name") {
		t.Errorf("discover empty-name err = %v, want 'empty universe name'", err)
	}
	if err := defaultNSReap(context.Background(), NSReap{Kind: KindAmp, Namespace: ""}); err == nil ||
		!strings.Contains(err.Error(), "empty namespace") {
		t.Errorf("amp empty-namespace err = %v, want 'empty namespace'", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// fakeCfg is a cloud.ConfigGetter that returns its region for any *.region key —
// the "config default" the plan's recorded region must override.
type fakeCfg struct{ region string }

func (f fakeCfg) GetString(key, def string) string {
	if f.region != "" && strings.HasSuffix(key, ".region") {
		return f.region
	}
	return def
}
func (fakeCfg) GetStringSlice(_ string, def []string) []string        { return def }
func (fakeCfg) GetDuration(_ string, def time.Duration) time.Duration { return def }

func fakeResolve(string) (string, error) { return "", nil }

// fakeConfirmVM is a cloud.VM whose confirmed reap reports a fixed instance
// count, so the zero-vs-positive confirmation guard can be exercised.
type fakeConfirmVM struct{ count int }

func (fakeConfirmVM) RunInstances(context.Context, cloud.RunSpec) ([]cloud.Instance, error) {
	return nil, nil
}
func (fakeConfirmVM) WaitRunning(context.Context, []string) error             { return nil }
func (fakeConfirmVM) WaitSSH(context.Context, []cloud.Instance, string) error { return nil }
func (fakeConfirmVM) Describe(context.Context, map[string]string) ([]cloud.Instance, error) {
	return nil, nil
}
func (fakeConfirmVM) Terminate(context.Context, []string) error      { return nil }
func (fakeConfirmVM) ReapByTag(context.Context, string) error        { return nil }
func (fakeConfirmVM) ReapStale(context.Context, time.Duration) error { return nil }
func (fakeConfirmVM) Connect(context.Context, cloud.Instance) (cloud.Conn, error) {
	return nil, nil
}
func (fakeConfirmVM) Meta() cloud.Meta { return cloud.Meta{} }
func (f fakeConfirmVM) ReapByTagConfirmed(context.Context, string) (int, error) {
	return f.count, nil
}

// TestExecuteReapsRecordedRegion: a plan with region A builds the cloud client
// for region A — the region the infra was created in — not the ambient config's
// region B. Reaping the wrong region is the silent-leak this closes.
func TestExecuteReapsRecordedRegion(t *testing.T) {
	orig := newCloudVM
	defer func() { newCloudVM = orig }()
	var seenRegion string
	newCloudVM = func(_ string, cfg cloud.ConfigGetter, _ cloud.SecretResolver) (cloud.VM, error) {
		seenRegion = cfg.GetString("apps.cloud.aws.region", "MISSING")
		return fakeConfirmVM{count: 1}, nil
	}
	p := &Plan{Cloud: &CloudReap{Cloud: "aws", RunTag: "t", Region: "eu-central-1"}}
	if err := Execute(context.Background(), p, fakeCfg{region: "us-west-2"}, fakeResolve, false); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seenRegion != "eu-central-1" {
		t.Fatalf("cloud client built for region %q, want the plan's eu-central-1 (not the config's us-west-2)", seenRegion)
	}
}

// TestExecuteZeroConfirmKeepsRecordUnlessForced: a reap that describes ZERO
// instances is NOT a confirmed reclaim (the region/creds may diverge) — Execute
// errors so the caller keeps the record; --force tolerates it.
func TestExecuteZeroConfirmKeepsRecordUnlessForced(t *testing.T) {
	orig := newCloudVM
	defer func() { newCloudVM = orig }()
	newCloudVM = func(string, cloud.ConfigGetter, cloud.SecretResolver) (cloud.VM, error) {
		return fakeConfirmVM{count: 0}, nil
	}
	p := &Plan{Cloud: &CloudReap{Cloud: "aws", RunTag: "t", Region: "us-west-2"}}

	err := Execute(context.Background(), p, fakeCfg{}, fakeResolve, false)
	if err == nil || !strings.Contains(err.Error(), "ZERO instances") {
		t.Fatalf("zero-confirm reap err = %v, want a 'described ZERO instances' error so the record is kept", err)
	}
	if ferr := Execute(context.Background(), p, fakeCfg{}, fakeResolve, true); ferr != nil {
		t.Fatalf("forced zero-confirm reap = %v, want nil (record may be dropped under --force)", ferr)
	}
}

// TestExecutePositiveConfirmSucceeds: a reap that acted on >=1 instance is a
// confirmed reclaim and returns nil (the record may be removed).
func TestExecutePositiveConfirmSucceeds(t *testing.T) {
	orig := newCloudVM
	defer func() { newCloudVM = orig }()
	newCloudVM = func(string, cloud.ConfigGetter, cloud.SecretResolver) (cloud.VM, error) {
		return fakeConfirmVM{count: 3}, nil
	}
	p := &Plan{Cloud: &CloudReap{Cloud: "aws", RunTag: "t", Region: "us-west-2"}}
	if err := Execute(context.Background(), p, fakeCfg{}, fakeResolve, false); err != nil {
		t.Fatalf("confirmed reap (3 instances) = %v, want nil", err)
	}
}
