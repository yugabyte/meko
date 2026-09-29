package helm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUpgradeArgs(t *testing.T) {
	tests := []struct {
		name       string
		release    Release
		valuesPath string
		want       []string
	}{
		{
			name:    "minimal",
			release: Release{Name: "ep-1", Chart: "chart", Namespace: "ns"},
			want:    []string{"upgrade", "--install", "ep-1", "chart", "--namespace", "ns"},
		},
		{
			name:       "with values",
			release:    Release{Name: "ep-1", Chart: "chart", Namespace: "ns"},
			valuesPath: "/tmp/values.yaml",
			want:       []string{"upgrade", "--install", "ep-1", "chart", "--namespace", "ns", "--values", "/tmp/values.yaml"},
		},
		{
			name:    "with wait",
			release: Release{Name: "ep-1", Chart: "chart", Namespace: "ns", Wait: true},
			want:    []string{"upgrade", "--install", "ep-1", "chart", "--namespace", "ns", "--wait"},
		},
		{
			name:    "with timeout",
			release: Release{Name: "ep-1", Chart: "chart", Namespace: "ns", Timeout: 90 * time.Second},
			want:    []string{"upgrade", "--install", "ep-1", "chart", "--namespace", "ns", "--timeout", "1m30s"},
		},
		{
			name:    "zero timeout omitted",
			release: Release{Name: "ep-1", Chart: "chart", Namespace: "ns", Timeout: 0},
			want:    []string{"upgrade", "--install", "ep-1", "chart", "--namespace", "ns"},
		},
		{
			name: "all options ordered",
			release: Release{
				Name:      "ep-1",
				Chart:     "chart",
				Namespace: "ns",
				Wait:      true,
				Timeout:   10 * time.Minute,
			},
			valuesPath: "/tmp/values.yaml",
			want: []string{
				"upgrade", "--install", "ep-1", "chart", "--namespace", "ns",
				"--values", "/tmp/values.yaml", "--wait", "--timeout", "10m0s",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeArgs(tt.release, tt.valuesPath)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("upgradeArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteValuesNil(t *testing.T) {
	path, cleanup, err := writeValues(nil)
	if err != nil {
		t.Fatalf("writeValues(nil): %v", err)
	}
	if cleanup == nil {
		t.Fatal("writeValues(nil) cleanup is nil; want a non-nil no-op")
	}
	if path != "" {
		t.Errorf("writeValues(nil) path = %q, want empty", path)
	}
	// Calling the no-op cleanup must be safe.
	cleanup()
}

func TestWriteValuesRoundTrip(t *testing.T) {
	value := map[string]any{
		"image": map[string]any{
			"repository": "repo/endpoint",
			"tag":        "latest",
		},
		"replicas": float64(3),
		"enabled":  true,
	}

	path, cleanup, err := writeValues(value)
	if err != nil {
		t.Fatalf("writeValues: %v", err)
	}
	if cleanup == nil {
		t.Fatal("cleanup is nil")
	}
	if path == "" {
		t.Fatal("path is empty for a non-nil value")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read values file: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal values file (JSON is a valid YAML subset): %v", err)
	}
	if !reflect.DeepEqual(got, value) {
		t.Errorf("round-trip mismatch: got %v, want %v", got, value)
	}

	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("cleanup did not remove %q: stat err = %v", path, err)
	}
}

func TestWriteValuesStructRoundTrip(t *testing.T) {
	type values struct {
		Name string `json:"name"`
		Port int    `json:"port"`
	}
	in := values{Name: "ep-abc", Port: 55433}

	path, cleanup, err := writeValues(in)
	if err != nil {
		t.Fatalf("writeValues: %v", err)
	}
	defer cleanup()
	if path == "" {
		t.Fatal("path is empty for a non-nil value")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read values file: %v", err)
	}
	var got values
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal values file: %v", err)
	}
	if got != in {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}
}

// A --wait deploy that runs out of time must say so in its own terms, not in
// Go's (#635) — the same exit status from any other cause is left alone.
func TestWaitTimeout(t *testing.T) {
	waiting := Release{Name: "ep-1", Wait: true, Timeout: 10 * time.Minute}
	timedOut := errors.New("helm upgrade --install ep-1: exit status 1: Error: context deadline exceeded")

	got := waitTimeout(waiting, timedOut).Error()
	if !strings.Contains(got, "did not become ready within 10m0s") {
		t.Errorf("waitTimeout() = %q, want it to name the wait budget", got)
	}
	if !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("waitTimeout() = %q, want helm's own text kept", got)
	}
	if !errors.Is(waitTimeout(waiting, timedOut), timedOut) {
		t.Error("waitTimeout() dropped the wrapped error")
	}

	other := errors.New("helm upgrade --install ep-1: exit status 1: Error: chart not found")
	if got := waitTimeout(waiting, other); got != other {
		t.Errorf("waitTimeout() rewrote a non-timeout failure: %v", got)
	}
	noWait := Release{Name: "ep-1"}
	if got := waitTimeout(noWait, timedOut); got != timedOut {
		t.Errorf("waitTimeout() rewrote a failure on a release that did not --wait: %v", got)
	}
}

// The text is helm's own, captured from a delete_cluster that followed its
// endpoint's delete_endpoint on meko-dev (helm 3.18.6, the controller image's).
const purgeRaceStderr = "Error: uninstallation completed with 1 error(s): uninstall: Failed to purge the release: release: not found"

func TestLostPurgeRace(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  string
		want bool
	}{
		{"purge lost to a concurrent uninstall", "helm uninstall yb-ep-1: exit status 1: " + purgeRaceStderr, true},
		{"release already marked uninstalled, purge lost", "helm uninstall yb-ep-1: exit status 1: " +
			"Error: uninstall: Failed to purge the release: release: not found", true},
		{"purge plus another error", "helm uninstall yb-ep-1: exit status 1: Error: uninstallation completed with 2 error(s): " +
			"failed to delete Service yb-ep-1: forbidden; uninstall: Failed to purge the release: release: not found", false},
		{"resource deletion failed", "helm uninstall yb-ep-1: exit status 1: Error: uninstallation completed with 1 error(s): " +
			"failed to delete Deployment yb-ep-1: forbidden", false},
		{"cluster unreachable", "helm uninstall yb-ep-1: exit status 1: Error: Kubernetes cluster unreachable", false},
	} {
		if got := lostPurgeRace(errors.New(tc.msg)); got != tc.want {
			t.Errorf("%s: lostPurgeRace() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fakeHelm writes a helm stand-in that prints stderr and exits with code.
func fakeHelm(t *testing.T, stderr string, code int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "helm")
	script := fmt.Sprintf("#!/bin/sh\necho %q >&2\nexit %d\n", stderr, code)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestUninstallToleratesLostPurgeRace(t *testing.T) {
	c := New(fakeHelm(t, purgeRaceStderr, 1), "", nil)
	if err := c.Uninstall(context.Background(), "yb-ep-1", "amp-dev", true, time.Minute); err != nil {
		t.Errorf("Uninstall() = %v, want nil: the release is gone", err)
	}

	c = New(fakeHelm(t, "Error: Kubernetes cluster unreachable", 1), "", nil)
	if err := c.Uninstall(context.Background(), "yb-ep-1", "amp-dev", true, time.Minute); err == nil {
		t.Error("Uninstall() = nil for an unreachable cluster, want the error")
	}
}
