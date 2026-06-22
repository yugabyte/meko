package helm

import (
	"encoding/json"
	"os"
	"reflect"
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
			name:    "create namespace",
			release: Release{Name: "ep-1", Chart: "chart", Namespace: "ns", CreateNamespace: true},
			want:    []string{"upgrade", "--install", "ep-1", "chart", "--namespace", "ns", "--create-namespace"},
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
				Name:            "ep-1",
				Chart:           "chart",
				Namespace:       "ns",
				CreateNamespace: true,
				Wait:            true,
				Timeout:         10 * time.Minute,
			},
			valuesPath: "/tmp/values.yaml",
			want: []string{
				"upgrade", "--install", "ep-1", "chart", "--namespace", "ns",
				"--create-namespace", "--values", "/tmp/values.yaml", "--wait", "--timeout", "10m0s",
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
