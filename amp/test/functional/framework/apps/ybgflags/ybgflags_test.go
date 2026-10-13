package ybgflags

import (
	"maps"
	"testing"
)

type fakeConfig map[string]string

func (f fakeConfig) GetString(key, def string) string {
	if v, ok := f[key]; ok {
		return v
	}
	return def
}

func TestStreamCompressionGzipIsTheBYOCSetting(t *testing.T) {
	master, tserver, err := StreamCompression(CompressionGzip)
	if err != nil {
		t.Fatal(err)
	}
	wantMaster := map[string]string{"enable_stream_compression": "true", "stream_compression_algo": "0"}
	wantTServer := map[string]string{"enable_stream_compression": "true", "stream_compression_algo": "1", "gzip_stream_compression_level": "1"}
	if !maps.Equal(master, wantMaster) {
		t.Errorf("master = %v, want %v", master, wantMaster)
	}
	if !maps.Equal(tserver, wantTServer) {
		t.Errorf("tserver = %v, want %v", tserver, wantTServer)
	}
}

func TestStreamCompressionNoneTurnsTheTServerAlgoOff(t *testing.T) {
	_, tserver, err := StreamCompression(CompressionNone)
	if err != nil {
		t.Fatal(err)
	}
	if tserver["stream_compression_algo"] != "0" {
		t.Errorf("tserver stream_compression_algo = %q, want 0", tserver["stream_compression_algo"])
	}
	if _, ok := tserver["gzip_stream_compression_level"]; ok {
		t.Errorf("none sets a gzip level: %v", tserver)
	}
}

func TestStreamCompressionEmptySetsNothing(t *testing.T) {
	master, tserver, err := StreamCompression("")
	if err != nil || len(master) != 0 || len(tserver) != 0 {
		t.Fatalf("StreamCompression(\"\") = %v, %v, %v; want two empty maps", master, tserver, err)
	}
}

func TestStreamCompressionRejectsAnUnknownMode(t *testing.T) {
	if _, _, err := StreamCompression("lz4"); err == nil {
		t.Fatal("lz4 accepted; an unknown mode must fail rather than deploy an uncompressed backing")
	}
}

func TestFromConfigDefaultsToGzip(t *testing.T) {
	_, tserver, err := FromConfig(fakeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if tserver["stream_compression_algo"] != "1" {
		t.Errorf("default tserver stream_compression_algo = %q, want 1", tserver["stream_compression_algo"])
	}
	if got := StreamCompressionMode(fakeConfig{StreamCompressionKey: " none "}); got != CompressionNone {
		t.Errorf("mode = %q, want none", got)
	}
}

func TestSignatureSplitsOnlyGzip(t *testing.T) {
	if Signature(CompressionGzip) == "" {
		t.Error("gzip must start its own perf series")
	}
	if Signature(CompressionNone) != "" || Signature("") != "" {
		t.Error("none and unset are YB's defaults and keep the existing series")
	}
}

func TestMergeLetsTheOverrideWin(t *testing.T) {
	base := map[string]string{"a": "1", "b": "2"}
	got := Merge(base, map[string]string{"b": "3"})
	if got["a"] != "1" || got["b"] != "3" {
		t.Errorf("Merge = %v", got)
	}
	if base["b"] != "2" {
		t.Error("Merge modified its base")
	}
}

func TestCSVIsSorted(t *testing.T) {
	if got := CSV(map[string]string{"z": "1", "a": "2"}); got != "a=2,z=1" {
		t.Errorf("CSV = %q", got)
	}
}
