// The server gflags every YugabyteDB the framework creates is given, whichever
// app deploys it. They are create-time settings: changing one is a rolling
// restart, so a backing carries them from its first start. A yba_universe built
// from a hand-written functional.yba.create_spec gets only what that body says.
package ybgflags

import (
	"fmt"
	"maps"
	"sort"
	"strings"
)

const StreamCompressionKey = "env.yb_stream_compression"

// BYOC deploys run gzip on AWS and GCP, where cross-zone transfer is billed,
// and none on Azure (meko#1859).
const (
	CompressionGzip = "gzip"
	CompressionNone = "none"
)

type Getter interface {
	GetString(key, def string) string
}

func StreamCompressionMode(get Getter) string {
	return strings.TrimSpace(get.GetString(StreamCompressionKey, CompressionGzip))
}

// StreamCompression returns the per-role gflags for mode. "" sets nothing and
// leaves the backing's own defaults.
func StreamCompression(mode string) (master, tserver map[string]string, err error) {
	master, tserver = map[string]string{}, map[string]string{}
	switch mode {
	case "":
		return master, tserver, nil
	case CompressionGzip:
		master = map[string]string{"enable_stream_compression": "true", "stream_compression_algo": "0"}
		tserver = map[string]string{"enable_stream_compression": "true", "stream_compression_algo": "1", "gzip_stream_compression_level": "1"}
		return master, tserver, nil
	case CompressionNone:
		master = map[string]string{"enable_stream_compression": "true", "stream_compression_algo": "0"}
		tserver = map[string]string{"enable_stream_compression": "true", "stream_compression_algo": "0"}
		return master, tserver, nil
	}
	return nil, nil, fmt.Errorf("%s=%q: want %q, %q or \"\" (leave the backing's defaults)", StreamCompressionKey, mode, CompressionGzip, CompressionNone)
}

func FromConfig(get Getter) (master, tserver map[string]string, err error) {
	return StreamCompression(StreamCompressionMode(get))
}

// Signature marks a perf series measured with compression on. none is YB's
// own default, so it keeps the series that predate the knob.
func Signature(mode string) string {
	if mode == CompressionGzip {
		return "+gzip"
	}
	return ""
}

// Merge returns base with over applied on top; neither input is modified.
func Merge(base, over map[string]string) map[string]string {
	out := maps.Clone(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, over)
	return out
}

// CSV renders flags as a sorted "k=v,k=v" list, the form yugabyted's
// --master_flags / --tserver_flags take.
func CSV(flags map[string]string) string {
	keys := make([]string, 0, len(flags))
	for k := range flags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + flags[k]
	}
	return strings.Join(parts, ",")
}
