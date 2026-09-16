package main

import "testing"

// A digest-pinned or registry-with-port reference has no tag worth showing, and an
// empty Version leaves the console's version column blank rather than printing a
// hostname or a digest as if it were a version.
func TestImageTag(t *testing.T) {
	for _, tc := range []struct{ image, want string }{
		{"quay.io/yugabyte/meko-api-server:0.260912.0-59d52fac1", "0.260912.0-59d52fac1"},
		{"nginx:1.27-alpine", "1.27-alpine"},
		// The colon belongs to the registry's port, so everything after it has a slash.
		{"localhost:5000/meko-ui", ""},
		{"quay.io/yugabyte/meko-ui", ""},
		{"quay.io/yugabyte/meko-ui@sha256:abc123", ""},
		{"", ""},
	} {
		if got := imageTag(tc.image); got != tc.want {
			t.Errorf("imageTag(%q) = %q, want %q", tc.image, got, tc.want)
		}
	}
}

// Exercises the mapping itself rather than restating it: an absent replicas field
// means one, a scaled-to-zero workload is omitted because the console reads total==0
// as an outage, and the version comes off the first container.
func TestParseWorkloads(t *testing.T) {
	raw := []byte(`{"items":[
	  {"metadata":{"name":"meko-api-server"},
	   "spec":{"replicas":1,"template":{"spec":{"containers":[{"image":"quay.io/yugabyte/meko-api-server:0.260912.0"}]}}},
	   "status":{"readyReplicas":1}},
	  {"metadata":{"name":"meko-ui"},
	   "spec":{"replicas":1,"template":{"spec":{"containers":[{"image":"quay.io/yugabyte/meko-ui:0.260912.0"}]}}},
	   "status":{}},
	  {"metadata":{"name":"no-replicas-field"},
	   "spec":{"template":{"spec":{"containers":[{"image":"nginx:1.27-alpine"}]}}},
	   "status":{"readyReplicas":1}},
	  {"metadata":{"name":"scaled-to-zero"},
	   "spec":{"replicas":0,"template":{"spec":{"containers":[{"image":"nginx:1.27-alpine"}]}}},
	   "status":{}},
	  {"metadata":{"name":"no-containers"},
	   "spec":{"replicas":2,"template":{"spec":{"containers":[]}}},
	   "status":{"readyReplicas":2}}
	]}`)

	got, err := parseWorkloads(raw)
	if err != nil {
		t.Fatalf("parseWorkloads: %v", err)
	}

	want := []struct {
		name           string
		healthy, total int
		version        string
	}{
		{"meko-api-server", 1, 1, "0.260912.0"},
		// readyReplicas absent is zero: the workload is down, which is the case the
		// tab exists for.
		{"meko-ui", 0, 1, "0.260912.0"},
		// Kubernetes omits replicas when it is nil; defaulting to 1 is what makes a
		// single-replica Deployment read 1/1 rather than 1/0.
		{"no-replicas-field", 1, 1, "1.27-alpine"},
		// scaled-to-zero is skipped entirely, so it never reaches the console's
		// total==0 branch, which paints red and lists it under "Waiting on".
		{"no-containers", 2, 2, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d components %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Name != w.name || g.Healthy != w.healthy || g.Total != w.total || g.Version != w.version {
			t.Errorf("component %d = %+v, want name=%s %d/%d version=%q",
				i, g, w.name, w.healthy, w.total, w.version)
		}
	}
}

// A namespace whose Role lacks the grant must cost one log line, not one per tick.
func TestComponentsLatchesOnForbidden(t *testing.T) {
	k := &k8s{compsForbidden: true}
	got, err := k.components()
	if err != nil || got != nil {
		t.Errorf("after a 403 latch: got (%v, %v), want (nil, nil) with no request attempted", got, err)
	}
}
