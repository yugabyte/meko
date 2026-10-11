package reap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

type marked struct {
	ns          string
	labels      map[string]string
	annotations map[string]string
}

func withFakeMarker(t *testing.T, fail string) *[]marked {
	t.Helper()
	orig, origDiscover := markNamespace, discoverNamespaces
	t.Cleanup(func() { markNamespace, discoverNamespaces = orig, origDiscover })
	discoverNamespaces = func(_ context.Context, n NSReap) ([]string, error) { return []string{n.Namespace}, nil }
	var got []marked
	markNamespace = func(_ context.Context, n NSReap, labels, annotations map[string]string) error {
		got = append(got, marked{ns: n.Namespace, labels: labels, annotations: annotations})
		if n.Namespace == fail {
			return errors.New("kubectl said no")
		}
		return nil
	}
	return &got
}

// A kept environment must be marked ON the namespace: the registry that records
// it is a file in the build's container, which CI destroys when the build ends.
func TestMarkKeptStampsEveryNamespaceWithTheExpiry(t *testing.T) {
	got := withFakeMarker(t, "")
	until := time.Date(2026, 8, 11, 2, 22, 2, 0, time.UTC)
	p := &Plan{Namespaces: []NSReap{
		{Kind: KindYBAStack, Namespace: "mekoqa-yba-1"},
		{Kind: KindDiscover, Namespace: "mekoqa-2"},
		{Kind: KindDiscover, Namespace: ""}, // nothing to mark
	}}
	if err := MarkKept(context.Background(), p, until); err != nil {
		t.Fatalf("MarkKept: %v", err)
	}
	if len(*got) != 2 {
		t.Fatalf("marked %d namespaces, want 2 (the unnamed one is skipped): %+v", len(*got), *got)
	}
	for _, m := range *got {
		if m.labels[kubemeta.KeepLabel] != kubemeta.KeepLabelValue {
			t.Errorf("%s: label %s = %q, want %q", m.ns, kubemeta.KeepLabel, m.labels[kubemeta.KeepLabel], kubemeta.KeepLabelValue)
		}
		if got := m.annotations[kubemeta.KeepUntil]; got != "2026-08-11T02:22:02Z" {
			t.Errorf("%s: %s = %q, want an RFC3339 expiry", m.ns, kubemeta.KeepUntil, got)
		}
	}
}

// No TTL means no expiry: the janitor reports such an environment rather than
// reaping it, so recording a bogus timestamp would be worse than none.
func TestMarkKeptWithoutATTLRecordsNoExpiry(t *testing.T) {
	got := withFakeMarker(t, "")
	p := &Plan{Namespaces: []NSReap{{Kind: KindDiscover, Namespace: "mekoqa-standing"}}}
	if err := MarkKept(context.Background(), p, time.Time{}); err != nil {
		t.Fatalf("MarkKept: %v", err)
	}
	m := (*got)[0]
	if m.labels[kubemeta.KeepLabel] != kubemeta.KeepLabelValue {
		t.Error("a standing environment must still be labelled kept")
	}
	if _, ok := m.annotations[kubemeta.KeepUntil]; ok {
		t.Errorf("recorded an expiry for an environment that has none: %v", m.annotations)
	}
}

// One namespace failing must not stop the rest: an unmarked namespace is one a
// janitor deletes, so every failure is attempted and then reported together.
func TestMarkKeptMarksTheRestAndReportsTheFailure(t *testing.T) {
	got := withFakeMarker(t, "mekoqa-bad")
	p := &Plan{Namespaces: []NSReap{
		{Kind: KindDiscover, Namespace: "mekoqa-bad"},
		{Kind: KindDiscover, Namespace: "mekoqa-good"},
	}}
	err := MarkKept(context.Background(), p, time.Now())
	if err == nil {
		t.Fatal("a namespace that could not be marked must be reported, not swallowed")
	}
	if len(*got) != 2 {
		t.Errorf("attempted %d namespaces, want 2, a failure must not abort the rest", len(*got))
	}
	if !strings.Contains(err.Error(), "mekoqa-bad") {
		t.Errorf("error %q does not name the namespace that went unmarked", err)
	}
}

func TestMarkKeptOnNothingIsNoOp(t *testing.T) {
	got := withFakeMarker(t, "")
	for _, p := range []*Plan{nil, {}} {
		if err := MarkKept(context.Background(), p, time.Now()); err != nil {
			t.Errorf("MarkKept(%v): %v", p, err)
		}
	}
	if len(*got) != 0 {
		t.Errorf("marked %d namespaces for an empty plan, want 0", len(*got))
	}
}

// A discover entry names a universe, not a namespace. YBA creates the namespace,
// and a kept stack whose universe namespace went unmarked lost its database.
func TestMarkKeptMarksTheNamespacesADiscoverEntryNames(t *testing.T) {
	got := withFakeMarker(t, "")
	discoverNamespaces = func(_ context.Context, n NSReap) ([]string, error) {
		return []string{"yb-dev-" + n.Namespace}, nil
	}
	p := &Plan{Namespaces: []NSReap{{Kind: KindDiscover, Namespace: "db-run1"}}}
	if err := MarkKept(context.Background(), p, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 || (*got)[0].ns != "yb-dev-db-run1" {
		t.Errorf("marked %+v, want yb-dev-db-run1", *got)
	}
}
