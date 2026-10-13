package features

import (
	"slices"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

func TestStreamCompressionContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		overrides []string
		want      []string
	}{
		{"default is gzip", nil, []string{
			"gflag:enable_stream_compression=true", "gflag:gzip_stream_compression_level=1", "gflag:stream_compression_algo=1"}},
		{"none", []string{"env.yb_stream_compression=none"}, []string{
			"gflag:enable_stream_compression=true", "gflag:stream_compression_algo=0"}},
		{"unset asks nothing", []string{"env.yb_stream_compression="}, nil},
		{"an attached backing is not ours", []string{"env.yb_deploy=external"}, nil},
		{"an explicit ampstack flag wins", []string{"env.ampstack.yb_tserver_gflags=stream_compression_algo=2"}, []string{
			"gflag:enable_stream_compression=true", "gflag:gzip_stream_compression_level=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := core.LoadConfig(nil, nil, tc.overrides)
			if err != nil {
				t.Fatal(err)
			}
			got, err := streamCompressionContract(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("contract = %v, want %v", got, tc.want)
			}
		})
	}
	cfg, err := core.LoadConfig(nil, nil, []string{"env.yb_stream_compression=lz4"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := streamCompressionContract(cfg); err == nil {
		t.Error("an unknown mode must fail the contract, not pass it vacuously")
	}
}
