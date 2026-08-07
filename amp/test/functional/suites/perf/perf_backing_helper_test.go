package perf

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/ampstack"
	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/pg"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
)

// deployInfo is a ybk8s.Info as Deploy returns it for an RF3 backing: the
// tserver Service, the per-pod DNS of every tserver, and the tailnet pod IPs.
func deployInfo() ybk8s.Info {
	return ybk8s.Info{
		Namespace:          "ampqa-yb-run7",
		YSQLPort:           5433,
		User:               "yugabyte",
		InClusterHostPorts: "yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433",
		TserverHostPorts: []string{
			"yb-tserver-0.yb-tservers.ampqa-yb-run7.svc.cluster.local:5433",
			"yb-tserver-1.yb-tservers.ampqa-yb-run7.svc.cluster.local:5433",
			"yb-tserver-2.yb-tservers.ampqa-yb-run7.svc.cluster.local:5433",
		},
		TailnetAddrs: []string{"10.1.2.3:5433", "10.1.2.4:5433", "10.1.2.5:5433"},
	}
}

// endpointConnFor is the connection the benchmarks drive for the deploy above.
func endpointConnFor() pg.ConnInfo {
	return endpointConn("ampqa-perf-cp-run7", "abc123", "10.9.9.9:55433")
}

// TestAmpBackingUsesPerPodTserverDNS is the meko#648 invariant: the amp
// controller (and through it every endpoint's thin client, which dials tserver
// RPC :9100 on the SAME host it gets for YSQL) must be wired at the per-pod
// stable DNS names. The tserver Service publishes only 5433, so handing it over
// crash-loops the endpoint on a heartbeat timeout.
func TestAmpBackingUsesPerPodTserverDNS(t *testing.T) {
	info := deployInfo()
	b := ampBacking(info)

	if b.Mode != ampstack.BackingExternal {
		t.Errorf("Mode = %q, want external", b.Mode)
	}
	want := strings.Join(info.TserverHostPorts, ",")
	if b.HostPorts != want {
		t.Errorf("HostPorts = %q, want the per-pod tserver DNS %q", b.HostPorts, want)
	}
	if strings.Contains(b.HostPorts, info.InClusterHostPorts) {
		t.Errorf("HostPorts must never carry the tserver Service: %q", b.HostPorts)
	}
	// The one-shot yb_storage CREATE runs from THIS process, which resolves only
	// the tailnet addresses.
	if b.AdminHostPorts != strings.Join(info.TailnetAddrs, ",") {
		t.Errorf("AdminHostPorts = %q, want the tailnet CSV", b.AdminHostPorts)
	}
	if b.User != "yugabyte" {
		t.Errorf("User = %q, want yugabyte", b.User)
	}
}

// TestEndpointConnUsesStableServiceDNS: the client pod reaches the endpoint
// cross-namespace by its Service DNS, never by the controller-reported address
// (a compute POD IP under the default ClusterIP service type) — only the port is
// taken from that address.
func TestEndpointConnUsesStableServiceDNS(t *testing.T) {
	ci := endpointConn("ampqa-perf-cp-run7", "abc123", "10.9.9.9:55433")
	if ci.Password != endpointComputePassword {
		t.Errorf("Password = %q, want %q — remote clients hit md5, not loopback trust", ci.Password, endpointComputePassword)
	}
	if ci.Host != "yb-ep-abc123.ampqa-perf-cp-run7.svc.cluster.local" {
		t.Errorf("Host = %q, want the endpoint Service DNS", ci.Host)
	}
	if ci.Port != 55433 {
		t.Errorf("Port = %d, want 55433", ci.Port)
	}
	if ci.User != endpointComputeUser || ci.Database != endpointComputeDB || ci.SSLMode != "disable" {
		t.Errorf("compute credentials = %+v", ci)
	}
	// A non-default compute port is honoured...
	if got := endpointConn("ns", "id", "yb-ep-id.ns.svc.cluster.local:6543").Port; got != 6543 {
		t.Errorf("Port = %d, want the reported 6543", got)
	}
	// ...and an address with no port (or none reported) falls back to the chart's.
	for _, addr := range []string{"", "10.9.9.9", "10.9.9.9:not-a-port"} {
		if got := endpointConn("ns", "id", addr).Port; got != endpointComputePort {
			t.Errorf("addr %q → port %d, want the %d fallback", addr, got, endpointComputePort)
		}
	}
}

// TestAmpStackConfigPinsNamespaceAndContext: each run (and each comparison
// variant) gets its OWN control-plane namespace, and the kube-context falls back
// to the backing's when env.ampstack.context is unset. The variant's own
// controller/endpoint images survive.
func TestAmpStackConfigPinsNamespaceAndContext(t *testing.T) {
	amp := ampstack.Config{ControllerImage: "amp-controller:branch", EndpointImage: "amp-endpoint:branch"}
	ybCfg := ybk8s.Config{KubeContext: "gke_perf"}

	got := ampStackConfig(amp, ybCfg, deployInfo(), "run7")
	if got.Namespace != "ampqa-perf-cp-run7" {
		t.Errorf("Namespace = %q", got.Namespace)
	}
	if got.Context != "gke_perf" {
		t.Errorf("Context = %q, want the backing's kube-context", got.Context)
	}
	if got.ControllerImage != "amp-controller:branch" || got.EndpointImage != "amp-endpoint:branch" {
		t.Errorf("variant amp images must survive: %+v", got)
	}
	// An explicit ampstack context wins over the backing's.
	amp.Context = "explicit"
	if got := ampStackConfig(amp, ybCfg, deployInfo(), "run7"); got.Context != "explicit" {
		t.Errorf("Context = %q, want the explicit env.ampstack.context", got.Context)
	}
	// Two variants never share a namespace.
	if ampCPNamespace("a-run7") == ampCPNamespace("b-run7") {
		t.Error("variant control-plane namespaces must differ")
	}
}

// TestEndpointPodSelectorMatchesWorkload: the metrics sampler points at the
// endpoint's compute pods by the label its workload stamps on them.
func TestEndpointPodSelectorMatchesWorkload(t *testing.T) {
	ep := ampEndpoint{EndpointID: "abc123"}
	if got := ep.podSelector(); got != "app.kubernetes.io/instance=yb-ep-abc123" {
		t.Errorf("podSelector = %q", got)
	}
}

// TestBackingConnServesThePresetOnly: the direct backing ConnInfo (tserver
// Service) still resolves for the setup-only pgbench preset.
func TestBackingConnServesThePresetOnly(t *testing.T) {
	ci := backingConn(deployInfo())
	if ci.Host != "yb-tserver-service.ampqa-yb-run7.svc.cluster.local" || ci.Port != 5433 {
		t.Errorf("Conn = %+v, want the tserver Service on 5433", ci)
	}
	if ci.User != "yugabyte" || ci.Database != "yugabyte" || ci.SSLMode != "disable" {
		t.Errorf("Conn credentials/db = %+v", ci)
	}
}

// TestTPCCWorkloadTargetsTheEndpointSingleHost: BenchBase's JDBC URL takes the
// single-host path — one endpoint means one host, no multi-host list and no
// loadBalanceHosts.
func TestTPCCWorkloadTargetsTheEndpointSingleHost(t *testing.T) {
	cfg := perfConfig{RemoteDir: "/opt/ampqa", Warehouses: 4, Terminals: 8, MaxHeap: "2g", Benchmark: "tpcc"}
	wl := tpccWorkloadFor(cfg, endpointConnFor()).(*tpccWorkload)
	seds := strings.Join(benchbase.BuildSeds(wl.baseSpec()), "\n")

	if !strings.Contains(seds, "s|localhost:5433|yb-ep-abc123.ampqa-perf-cp-run7.svc.cluster.local:55433|g") {
		t.Errorf("tpcc must patch in the single endpoint address:\n%s", seds)
	}
	if strings.Contains(seds, "loadBalanceHosts") {
		t.Errorf("a single endpoint needs no loadBalanceHosts:\n%s", seds)
	}
	if strings.Contains(seds, "yb-tserver") {
		t.Errorf("no tserver may appear in the workload URL:\n%s", seds)
	}
}

// TestEveryWorkloadDrivesTheEndpointConn: pgbench and sysbench carry exactly the
// connection they were built with, so no driver can quietly keep talking to the
// backing.
func TestEveryWorkloadDrivesTheEndpointConn(t *testing.T) {
	conn := endpointConnFor()
	if got := pgbenchWorkloadFor(pgbenchParams{Scale: 10}, conn).(*pgbenchWorkload).conn; got != conn {
		t.Errorf("pgbench conn = %+v, want %+v", got, conn)
	}
	if got := sysbenchWorkloadFor(sysbenchParams{Script: "oltp_read_write"}, conn).(*sysbenchWorkload).conn; got != conn {
		t.Errorf("sysbench conn = %+v, want %+v", got, conn)
	}
}

// TestLoadSignatureIsDeterministicAndConnectionFree: the signature must be
// reproducible for an identical load and must no longer carry any
// connection-shape field — there is one target now, the compute endpoint, so
// nothing about the connection can vary between two runs of the same load.
func TestLoadSignatureIsDeterministicAndConnectionFree(t *testing.T) {
	bc := BenchConfig{Iterations: 3}
	cfg := perfConfig{RF: 3, Warehouses: 4}

	if LoadSignature(loadFieldsTPCC(bc, cfg)) != LoadSignature(loadFieldsTPCC(bc, cfg)) {
		t.Error("tpcc load signature must be deterministic")
	}
	if strings.Contains(marshalLoad(t, loadFieldsTPCC(bc, cfg)), "connect_mode") {
		t.Error("the load signature must not carry a connection mode")
	}
	// A real load change still moves it.
	heavier := perfConfig{RF: 3, Warehouses: 8}
	if LoadSignature(loadFieldsTPCC(bc, cfg)) == LoadSignature(loadFieldsTPCC(bc, heavier)) {
		t.Error("a different warehouse count must change the load signature")
	}
	params := pgbenchParams{Scale: 10, Clients: 8, Jobs: 2}
	if LoadSignature(loadFieldsPgbench(bc, cfg, params)) != LoadSignature(loadFieldsPgbench(bc, cfg, params)) {
		t.Error("pgbench load signature must be deterministic")
	}
	sp := sysbenchParams{Script: "oltp_read_write", Tables: 4, TableSize: 100000, Threads: 8}
	if LoadSignature(loadFieldsSysbench(bc, cfg, sp)) != LoadSignature(loadFieldsSysbench(bc, cfg, sp)) {
		t.Error("sysbench load signature must be deterministic")
	}
}

// marshalLoad renders the LoadFields exactly as LoadSignature hashes them, so a
// test can assert on which knobs the signature actually carries.
func marshalLoad(t *testing.T, f LoadFields) string {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal load fields: %v", err)
	}
	return string(b)
}
