package loadtest

import (
	"math"
	"regexp"
	"testing"
)

func TestParseExpositionKeepsCountersAndHistograms(t *testing.T) {
	body := `# HELP meko_http_requests_total requests
# TYPE meko_http_requests_total counter
meko_http_requests_total{method="GET",route="/api/v1/datapacks",status="200"} 12
meko_http_requests_total{method="GET",route="/api/v1/datapacks",status="500"} 1
meko_http_request_duration_seconds_bucket{method="GET",route="/api/v1/datapacks",le="0.1"} 10
meko_http_request_duration_seconds_count{method="GET",route="/api/v1/datapacks"} 13
meko_http_request_duration_seconds_sum{method="GET",route="/api/v1/datapacks"} 0.65
meko_http_requests_inflight 3
process_cpu_seconds_total NaN
`
	got := parseExposition(body, nil)
	want := map[string]float64{
		"meko_http_requests_total|method=GET,route=/api/v1/datapacks,status=200":      12,
		"meko_http_requests_total|method=GET,route=/api/v1/datapacks,status=500":      1,
		"meko_http_request_duration_seconds_count|method=GET,route=/api/v1/datapacks": 13,
		"meko_http_request_duration_seconds_sum|method=GET,route=/api/v1/datapacks":   0.65,
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestParseExpositionWithKeepDropsLabelsAndSums(t *testing.T) {
	body := `handler_latency_yb_tserver_TabletServerService_Read_count{table_id="a"} 4
handler_latency_yb_tserver_TabletServerService_Read_count{table_id="b"} 6
handler_latency_yb_tserver_TabletServerService_Read_sum{table_id="a"} 400
rpc_inbound_calls_created{metric_type="server"} 9
rocksdb_bytes_read{table_id="a"} 99
`
	got := parseExposition(body, regexp.MustCompile(`^handler_latency_yb_tserver_TabletServerService_Read_(count|sum)$|^rpc_inbound_calls_created$`))
	if got["handler_latency_yb_tserver_TabletServerService_Read_count|"] != 10 || got["handler_latency_yb_tserver_TabletServerService_Read_sum|"] != 400 ||
		got["rpc_inbound_calls_created_total|"] != 9 || len(got) != 3 {
		t.Errorf("parsed %v", got)
	}
}

func TestPodMetricsUnits(t *testing.T) {
	raw := `{"items":[{"metadata":{"name":"meko-api-server-abc-1"},"containers":[{"usage":{"cpu":"250m","memory":"128Mi"}},{"usage":{"cpu":"500000000n","memory":"1048576Ki"}}]}]}`
	got := parsePodMetrics(raw)["meko-api-server-abc-1"]
	if math.Abs(got.cpuMilli-750) > 0.001 || math.Abs(got.memMiB-1152) > 0.001 {
		t.Errorf("usage %+v, want 750 mcores and 1152 MiB", got)
	}
	for q, want := range map[string]float64{"2": 2000, "1500u": 1.5, "0": 0} {
		if got := cpuMilli(q); math.Abs(got-want) > 0.001 {
			t.Errorf("cpuMilli(%q) = %v, want %v", q, got, want)
		}
	}
	if got := memMiB("1Gi"); got != 1024 {
		t.Errorf("memMiB(1Gi) = %v", got)
	}
}

func TestComponentNamesAPodByItsWorkload(t *testing.T) {
	for pod, want := range map[string]string{
		"meko-api-server-596996954c-769dc":       "meko-api-server",
		"rag-agent-conversations-6f44b46c55-nj9": "rag-agent-conversations",
		"yb-zone-a-0":                            "yugabyted",
		"mekoload-1a2b-0":                        "mekoload",
		"something-else-7f9":                     "something-else",
	} {
		if got := component(pod); got != want {
			t.Errorf("component(%q) = %q, want %q", pod, got, want)
		}
	}
}
