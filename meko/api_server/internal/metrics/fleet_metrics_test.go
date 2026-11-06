package metrics

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFleetMetrics_GroupsTheRegistry(t *testing.T) {
	hb := time.Unix(1_800_000_000, 0)
	c := newFleetMetricsCollector(FleetSources{
		Environments: func(context.Context) ([]FleetEnvironment, error) {
			return []FleetEnvironment{
				{UUID: "u1", Kind: "amp", Type: "BYOC", State: "READY", AgentVersion: "1.2", LastHeartbeatAt: &hb},
				{UUID: "u2", Kind: "amp", Type: "BYOC", State: "READY", AgentVersion: "1.2", LastHeartbeatAt: &hb},
				{UUID: "u3", Kind: "meko", Type: "BYOC", State: "PENDING"},
				{UUID: "u4", Kind: "amp", Type: "MANAGED", State: "DEREGISTERED", AgentVersion: "1.0", LastHeartbeatAt: &hb},
			}, nil
		},
		TasksByStatus: func(context.Context) (map[string]int64, error) {
			return map[string]int64{"running": 2, "failed": 1}, nil
		},
	})
	gather := gatherAdmin(t, c)
	syncRefresh(c)
	got := gather()

	want := map[string]float64{
		"meko_api_ycp_environments{kind=amp}{state=READY}{type=BYOC}":                      2,
		"meko_api_ycp_environments{kind=meko}{state=PENDING}{type=BYOC}":                   1,
		"meko_api_ycp_environments{kind=amp}{state=DEREGISTERED}{type=MANAGED}":            1,
		"meko_api_ycp_environment_last_heartbeat_timestamp_seconds{env_uuid=u1}{kind=amp}": 1_800_000_000,
		"meko_api_ycp_environment_last_heartbeat_timestamp_seconds{env_uuid=u2}{kind=amp}": 1_800_000_000,
		"meko_api_ycp_environments_by_agent_version{agent_version=1.2}{kind=amp}":          2,
		"meko_api_ycp_environments_by_agent_version{agent_version=unknown}{kind=meko}":     1,
		"meko_api_ycp_environment_tasks{status=running}":                                   2,
		"meko_api_ycp_environment_tasks{status=failed}":                                    1,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d series, want %d: %v", len(got), len(want), got)
	}
}

// A deregistered environment's agent is gone; exporting its frozen heartbeat would
// page on it forever, and its version would skew the fleet's version mix.
func TestFleetMetrics_DeregisteredHasNoHeartbeatOrVersion(t *testing.T) {
	hb := time.Unix(1, 0)
	samples := fleetSamples([]FleetEnvironment{
		{UUID: "gone", Kind: "amp", Type: "BYOC", State: "DEREGISTERED", AgentVersion: "0.9", LastHeartbeatAt: &hb},
	})
	for _, s := range samples {
		if s.desc != fleetEnvironmentsDesc {
			t.Errorf("deregistered environment emitted %s", s.desc)
		}
	}
}

func TestFleetMetrics_FailedProbeDropsOnlyItsSeries(t *testing.T) {
	c := newFleetMetricsCollector(FleetSources{
		Environments: func(context.Context) ([]FleetEnvironment, error) { return nil, errors.New("db down") },
		TasksByStatus: func(context.Context) (map[string]int64, error) {
			return map[string]int64{"pending": 3}, nil
		},
	})
	gather := gatherAdmin(t, c)
	syncRefresh(c)
	got := gather()
	if len(got) != 1 || got["meko_api_ycp_environment_tasks{status=pending}"] != 3 {
		t.Fatalf("got %v, want only the task series", got)
	}
}

func TestWithFleet_NoSourcesRegistersNothing(t *testing.T) {
	m := New(nil, nil)
	before, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	m.WithFleet(FleetSources{})
	after, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatalf("empty FleetSources changed the registry: %d -> %d families", len(before), len(after))
	}
	var nilM *Metrics
	if nilM.WithFleet(FleetSources{}) != nil {
		t.Fatal("nil receiver should stay nil")
	}
}
