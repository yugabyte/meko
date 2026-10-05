package service

import (
	"encoding/json"
	"fmt"
	"time"
)

// telemetryComponentName is the synthetic component a telemetry problem is
// reported as. Reusing the component list rather than adding a column: the console
// already renders a component's Error, so a 0/1 "telemetry" row with the reason on
// it lands where an operator is already looking, and needs no migration.
const telemetryComponentName = "telemetry"

// storedComponents decodes what is already on the environment, so a telemetry row
// can be folded into it rather than written over it. An agent that reported no
// components — every amp agent, and any agent whose project failed — must not
// lose the last good report just because the control plane has something to add.
// Unreadable stored JSON yields nil, which degrades to "record only the telemetry
// row" rather than failing the whole status write.
func storedComponents(raw []byte) []EnvComponent {
	if len(raw) == 0 {
		return nil
	}
	var comps []EnvComponent
	if err := json.Unmarshal(raw, &comps); err != nil {
		return nil
	}
	return comps
}

// hasTelemetryComponent reports whether a stored list still carries the synthetic
// row. It is what lets the row be REMOVED again: for an amp environment the agent
// never reports components, so once a telemetry problem clears there is nothing else
// to tell ApplyStatus that the stored list needs rewriting, and the row would latch
// on for the life of the environment.
func hasTelemetryComponent(comps []EnvComponent) bool {
	for _, c := range comps {
		if c.Name == telemetryComponentName {
			return true
		}
	}
	return false
}

// withTelemetryComponent returns the agent's reported components plus a synthetic
// telemetry row when there is something to say. An empty reason returns the
// components unchanged, so a healthy environment grows no extra row.
func withTelemetryComponent(comps []EnvComponent, reason string) []EnvComponent {
	if reason == "" {
		return comps
	}
	// Replace rather than append if the agent somehow reported one: two rows named
	// "telemetry" in the console would be worse than either alone.
	out := make([]EnvComponent, 0, len(comps)+1)
	for _, c := range comps {
		if c.Name != telemetryComponentName {
			out = append(out, c)
		}
	}
	return append(out, EnvComponent{Name: telemetryComponentName, Healthy: 0, Total: 1, Error: reason})
}

// Telemetry is an agent's account of where its deployment's metrics and logs
// actually go, read from the env the chart set from the same values its collectors
// render from. The wire twin is ycp/internal/api.Telemetry.
//
// It exists because a BYOC install runs from a checkout the customer has: values,
// templates and every rule inside the chart are theirs to edit, so nothing the
// chart enforces is actually enforced. The metrics endpoint is not the customer's
// to choose — it is the door credentials were minted for — and this control plane
// is the only party in the loop they do not control.
type Telemetry struct {
	OtelEnabled     bool   `json:"otel_enabled"`
	ByocEnabled     bool   `json:"byoc_enabled"`
	MetricsEndpoint string `json:"metrics_endpoint,omitempty"`
	LogsSink        string `json:"logs_sink,omitempty"`

	// Runtime, not configuration: what the collectors are actually achieving. The
	// fields above can all be correct while the environment ships nothing.
	//
	// StalledSeconds is the detection signal — how long the byoc exporter's
	// sent-counter has sat still, as the AGENT measured it. The delta has to be
	// computed where the polling already happens; doing it here would mean storing
	// the previous reading per environment. The collectors scrape on a fixed clock,
	// so a healthy counter climbs whether or not the environment is busy, which is
	// what makes "flat" mean broken rather than idle.
	//
	// The other three are diagnosis, not detection: MetricsQueued splits "nothing is
	// reaching the exporter" from "the door is not taking it", and MetricsSendFailed
	// only moves after the exporter's retry budget is spent.
	MetricsSent       float64 `json:"metrics_sent,omitempty"`
	MetricsSendFailed float64 `json:"metrics_send_failed,omitempty"`
	MetricsQueued     float64 `json:"metrics_queued,omitempty"`
	StalledSeconds    int     `json:"stalled_seconds,omitempty"`
	ProbeError        string  `json:"probe_error,omitempty"`
}

// TelemetryPolicy is what this control plane will accept. MetricsURL is the same
// value the install hint prints, which is the whole point of it being one config
// field: the endpoint the console tells an operator to use and the endpoint the
// enroll check accepts cannot be two decisions, or the hint sends people into a
// rejection.
//
// Telemetry is a SOFT requirement at the control plane: nothing here refuses a
// registration or an enrolment. An environment that cannot ship metrics is still an
// environment an operator needs to see, and one we would rather have registered and
// visibly degraded than turned away and invisible — a refusal hides the very thing
// it is complaining about.
//
// Nothing else enforces it either, for now: the chart warns through NOTES.txt and
// its post-install preflight reports to its own log, because the BYOC door is still
// being rolled out fleet by fleet. So this is the whole of it — a telemetry problem
// is visible everywhere and blocking nowhere, deliberately, until the door is
// everywhere and the render checks and the probe go back to failing.
type TelemetryPolicy struct {
	MetricsURL string
	// StaleAfter is how long the byoc exporter's sent-counter may sit still before
	// the environment is degraded. Zero disables the runtime check, which is what a
	// control plane wants when its agents are too old to report counters.
	//
	// It is generous on purpose. The collectors scrape on a 15s clock so a healthy
	// counter climbs constantly, but the exporter retries a failed send for 300s
	// before giving up, and a rolling collector restart is a legitimate gap. A
	// threshold near the scrape interval would flap; this is looking for "stopped",
	// not "hiccuped".
	StaleAfter time.Duration
}

// DefaultTelemetryStaleAfter is the shipped threshold. See TelemetryPolicy.StaleAfter.
const DefaultTelemetryStaleAfter = 10 * time.Minute

// SetTelemetryPolicy configures the enroll-time attestation check. The zero policy
// (no MetricsURL) checks nothing, which is what a control plane with no BYOC door
// configured wants.
func (s *EnvironmentService) SetTelemetryPolicy(p TelemetryPolicy) {
	s.telemetry = p
}

// BYOCMetricsURL is the door this control plane issues, for the install hint to
// print. Read from the policy rather than passed to the hint separately: the
// endpoint an operator is told to use and the endpoint enrol accepts have to be one
// value, or the console hands out a command it will then refuse.
func (s *EnvironmentService) BYOCMetricsURL() string {
	return s.telemetry.MetricsURL
}

// check reads a configuration attestation and returns why this environment should
// be marked degraded, or "" when there is nothing to say. It cannot refuse: see
// TelemetryPolicy.
//
// Applied identically at enrolment and on every status report, so an environment
// that ships nowhere is degraded from the moment it checks in rather than from its
// first heartbeat, and a deployment edited later is caught on the next tick.
func (p TelemetryPolicy) check(kind string, t *Telemetry) (degraded string) {
	if !attests(kind, t) || p.MetricsURL == "" {
		return ""
	}
	if t == nil {
		return "the agent reports no telemetry configuration (an image predating telemetry attestation); " +
			"redeploy with a current agent image so this environment's metrics can be verified"
	}

	var reason string
	switch {
	case !t.OtelEnabled:
		reason = "otel.enabled is false in this deployment, so nothing collects metrics or logs"
	case !t.ByocEnabled:
		reason = "otel.byoc.enabled is false in this deployment, so its metrics never leave the cluster"
	case t.MetricsEndpoint != p.MetricsURL:
		reason = fmt.Sprintf("this deployment ships metrics to %q, but this control plane issued %q",
			t.MetricsEndpoint, p.MetricsURL)
	default:
		return ""
	}
	return reason + ". Re-run the install command from the install hint this console prints for the environment"
}

// attests reports whether an environment's telemetry report means anything. An amp
// agent's always does. A meko chart only gained a collector later, so an agent on
// an older one reports nothing — silence there is a chart predating the collector,
// not a broken one, and degrading every such environment at control-plane rollout
// would say nothing an operator can act on. Once it does report, it is held to the
// same checks as amp.
func attests(kind string, t *Telemetry) bool {
	switch kind {
	case KindAmp:
		return true
	case KindMeko:
		return t != nil
	}
	return false
}

// checkRuntime asks the other question: this deployment is configured correctly,
// but is it actually shipping? Configuration cannot answer that — the door goes
// unreachable, a password is rotated, PrivateLink breaks, a collector crashloops,
// and every field check() looks at still says "fine".
//
// DEGRADE-ONLY, never a refusal, even on a first enrolment. A door outage is not
// the operator's misconfiguration and must not stop a new environment coming up;
// it also clears itself, unlike a wrong endpoint baked into a release.
func (p TelemetryPolicy) checkRuntime(kind string, t *Telemetry) string {
	if !attests(kind, t) || p.MetricsURL == "" || p.StaleAfter <= 0 || t == nil {
		return ""
	}
	// The agent could not read its collector at all. Worth saying — an unreachable
	// collector Service is a real problem — but it is "we cannot see", not "metrics
	// have stopped", so it reads differently from a confirmed stall.
	if t.ProbeError != "" {
		return "cannot read the collectors' own telemetry, so whether this environment is still " +
			"shipping metrics is unknown: " + t.ProbeError
	}
	stalled := time.Duration(t.StalledSeconds) * time.Second
	if stalled < p.StaleAfter {
		return ""
	}

	// Which half of the pipeline, from the two diagnostic counters. With the
	// sent-counter flat, an empty queue means nothing is reaching the exporter (the
	// fault is upstream — the receiver is scraping nothing) and a non-empty one
	// means the door is not taking what we have.
	where := "nothing is reaching the exporter, so the collectors are scraping nothing — check the collector pods and their targets"
	if t.MetricsQueued > 0 {
		where = fmt.Sprintf("%.0f point(s) are queued and not draining, so the door is not accepting them — check connectivity to %s and the metrics credential",
			t.MetricsQueued, p.MetricsURL)
	}
	if t.MetricsSendFailed > 0 {
		where += fmt.Sprintf(" (%.0f point(s) have already failed permanently)", t.MetricsSendFailed)
	}
	return fmt.Sprintf("this environment has shipped no metrics for %s: %s", stalled.Round(time.Minute), where)
}
