package metrics

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Fleet metrics are the YCP control plane's view of the environments it manages,
// the counterpart to the reconcile-agent's own ycp_agent_* series: the agent says
// why its loop is failing, these say which environments YCP has stopped hearing
// from, what state the fleet is in, and which agent builds it runs. They ride the
// admin collector's cache, so they refresh at most once a minute and a slow
// registry query never stalls a scrape.
//
// Every replica reports the whole fleet; aggregate with max, never sum.

const deregistered = "DEREGISTERED"

var (
	fleetEnvironmentsDesc = prometheus.NewDesc(namespace+"_ycp_environments",
		"Registered environments by kind, type and state, deregistered ones included. One series "+
			"per replica: aggregate with max. Refreshed at most once a minute.",
		[]string{"kind", "type", "state"}, nil)
	fleetHeartbeatDesc = prometheus.NewDesc(namespace+"_ycp_environment_last_heartbeat_timestamp_seconds",
		"Unix time of each environment's last agent heartbeat (the agent sends one every ~10s). "+
			"Not deregistered, and only once a first heartbeat has landed. Alert on time() minus "+
			"this; up to a minute stale by construction. One series per replica: aggregate with max.",
		[]string{"env_uuid", "kind"}, nil)
	fleetAgentVersionsDesc = prometheus.NewDesc(namespace+"_ycp_environments_by_agent_version",
		"Environments that are not deregistered, by kind and the agent version they last reported "+
			"('unknown' before the first report). One series per replica: aggregate with max.",
		[]string{"kind", "agent_version"}, nil)
	fleetTasksDesc = prometheus.NewDesc(namespace+"_ycp_environment_tasks",
		"Controller tasks relayed by agents, by status. Terminal statuses count only what "+
			"retention has not yet pruned. One series per replica: aggregate with max.",
		[]string{"status"}, nil)
)

// FleetEnvironment is one environment as the fleet metrics see it.
type FleetEnvironment struct {
	LastHeartbeatAt *time.Time
	UUID            string
	Kind            string
	Type            string
	State           string
	AgentVersion    string
}

// FleetSources are the control plane's registry reads. A nil source is skipped.
type FleetSources struct {
	Environments func(ctx context.Context) ([]FleetEnvironment, error)
	// TasksByStatus returns relayed-task counts keyed by status.
	TasksByStatus func(ctx context.Context) (map[string]int64, error)
}

func newFleetMetricsCollector(src FleetSources) *adminMetricsCollector {
	c := &adminMetricsCollector{now: time.Now}
	if f := src.Environments; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{fleetEnvironmentsDesc, fleetHeartbeatDesc, fleetAgentVersionsDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				envs, err := f(ctx)
				if err != nil {
					return nil, err
				}
				return fleetSamples(envs), nil
			},
		})
	}
	if f := src.TasksByStatus; f != nil {
		c.probes = append(c.probes, &adminProbe{
			descs: []*prometheus.Desc{fleetTasksDesc},
			read: func(ctx context.Context) ([]adminSample, error) {
				byStatus, err := f(ctx)
				if err != nil {
					return nil, err
				}
				var out []adminSample
				for status, n := range byStatus {
					out = append(out, adminSample{desc: fleetTasksDesc, labels: []string{status}, value: float64(n)})
				}
				return out, nil
			},
		})
	}
	return c
}

func fleetSamples(envs []FleetEnvironment) []adminSample {
	type stateKey struct{ kind, typ, state string }
	type versionKey struct{ kind, version string }
	states := map[stateKey]int{}
	versions := map[versionKey]int{}
	var out []adminSample
	for _, e := range envs {
		states[stateKey{e.Kind, e.Type, e.State}]++
		if e.State == deregistered {
			continue
		}
		v := e.AgentVersion
		if v == "" {
			v = "unknown"
		}
		versions[versionKey{e.Kind, v}]++
		if e.LastHeartbeatAt != nil {
			out = append(out, adminSample{
				desc: fleetHeartbeatDesc, labels: []string{e.UUID, e.Kind},
				value: float64(e.LastHeartbeatAt.UnixNano()) / 1e9,
			})
		}
	}
	for k, n := range states {
		out = append(out, adminSample{
			desc: fleetEnvironmentsDesc, labels: []string{k.kind, k.typ, k.state}, value: float64(n),
		})
	}
	for k, n := range versions {
		out = append(out, adminSample{
			desc: fleetAgentVersionsDesc, labels: []string{k.kind, k.version}, value: float64(n),
		})
	}
	return out
}

// WithFleet adds the YCP fleet gauges. Only for a control plane, which is the only
// mode with an environment registry. Chainable and nil-safe, like WithAdminBusiness.
func (m *Metrics) WithFleet(src FleetSources) *Metrics {
	if m == nil {
		return m
	}
	c := newFleetMetricsCollector(src)
	if len(c.probes) == 0 {
		return m
	}
	m.reg.MustRegister(c)
	return m
}
