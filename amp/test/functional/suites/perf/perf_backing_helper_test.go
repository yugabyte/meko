package perf

import (
	"reflect"
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/apps/benchbase"
	"github.com/yugabyte/amp/test/functional/framework/apps/ybk8s"
)

// deployInfo is a ybk8s.Info as Deploy returns it for an RF3 backing: the
// tserver Service plus the per-pod DNS of every tserver.
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
	}
}

func TestTargetForCarriesServiceConnAndEveryTserver(t *testing.T) {
	tgt := targetFor(deployInfo())
	if tgt.Conn.Host != "yb-tserver-service.ampqa-yb-run7.svc.cluster.local" || tgt.Conn.Port != 5433 {
		t.Errorf("Conn = %+v, want the tserver Service on 5433", tgt.Conn)
	}
	if tgt.Conn.User != "yugabyte" || tgt.Conn.Database != "yugabyte" || tgt.Conn.SSLMode != "disable" {
		t.Errorf("Conn credentials/db = %+v", tgt.Conn)
	}
	if !reflect.DeepEqual(tgt.Tservers, deployInfo().TserverHostPorts) {
		t.Errorf("Tservers = %v", tgt.Tservers)
	}
}

func TestBenchAddrsByConnectMode(t *testing.T) {
	tgt := targetFor(deployInfo())
	if got := tgt.benchAddrs(ConnectAllTservers); len(got) != 3 {
		t.Errorf("all_tservers → %v, want all 3 tservers", got)
	}
	// service mode (and any unknown value) → nil, i.e. "use the single Conn host".
	for _, mode := range []string{ConnectService, "", "nonsense"} {
		if got := tgt.benchAddrs(mode); got != nil {
			t.Errorf("mode %q → %v, want nil (Service only)", mode, got)
		}
	}
	// A deploy that resolved no per-pod list falls back to the Service too.
	bare := backingTarget{Conn: tgt.Conn}
	if got := bare.benchAddrs(ConnectAllTservers); got != nil {
		t.Errorf("no tserver list → %v, want nil", got)
	}
}

// TestTPCCWorkloadTargetsEveryTserver: in all_tservers mode the generated
// BenchBase config patch names every tserver in the JDBC URL and balances over
// them; in service mode it patches in the single Service address, unchanged from
// before.
func TestTPCCWorkloadTargetsEveryTserver(t *testing.T) {
	cfg := perfConfig{RemoteDir: "/opt/ampqa", Warehouses: 4, Terminals: 8, MaxHeap: "2g", Benchmark: "tpcc"}
	tgt := targetFor(deployInfo())

	cfg.ConnectMode = ConnectAllTservers
	all := tpccWorkloadFor(cfg, tgt).(*tpccWorkload)
	seds := strings.Join(benchbase.BuildSeds(all.baseSpec()), "\n")
	for _, want := range []string{
		"yb-tserver-0.yb-tservers.ampqa-yb-run7.svc.cluster.local:5433,yb-tserver-1",
		"loadBalanceHosts=true",
	} {
		if !strings.Contains(seds, want) {
			t.Errorf("all_tservers seds missing %q:\n%s", want, seds)
		}
	}

	cfg.ConnectMode = ConnectService
	svc := tpccWorkloadFor(cfg, tgt).(*tpccWorkload)
	seds = strings.Join(benchbase.BuildSeds(svc.baseSpec()), "\n")
	if !strings.Contains(seds, "s|localhost:5433|yb-tserver-service.ampqa-yb-run7.svc.cluster.local:5433|g") {
		t.Errorf("service mode should patch the single Service address:\n%s", seds)
	}
	if strings.Contains(seds, "loadBalanceHosts") {
		t.Errorf("service mode needs no loadBalanceHosts:\n%s", seds)
	}
}

// TestLoadSignatureTracksConnectMode: the connect mode is part of perf/tpcc's
// load signature, so a baseline taken under one mode is not compared against a
// run under the other. pgbench/sysbench targeting cannot vary, so their
// signatures must not move.
func TestLoadSignatureTracksConnectMode(t *testing.T) {
	bc := BenchConfig{Iterations: 3}
	all := perfConfig{RF: 3, Warehouses: 4, ConnectMode: ConnectAllTservers}
	svc := perfConfig{RF: 3, Warehouses: 4, ConnectMode: ConnectService}
	if LoadSignature(loadFieldsTPCC(bc, all)) == LoadSignature(loadFieldsTPCC(bc, svc)) {
		t.Errorf("tpcc load signature must differ across connect modes")
	}
	if LoadSignature(loadFieldsPgbench(bc, all, pgbenchParams{})) != LoadSignature(loadFieldsPgbench(bc, svc, pgbenchParams{})) {
		t.Errorf("pgbench load signature must not depend on connect_mode")
	}
	sp := sysbenchParams{}
	if LoadSignature(loadFieldsSysbench(bc, all, sp)) != LoadSignature(loadFieldsSysbench(bc, svc, sp)) {
		t.Errorf("sysbench load signature must not depend on connect_mode")
	}
}
