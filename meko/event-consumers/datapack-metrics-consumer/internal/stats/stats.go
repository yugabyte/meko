// Package stats holds the consumer's metrics, served at /metrics.
//
// They are always recorded: the consumer exists to be watched, and its lag gauge is the alert for the
// main failure mode, events arriving with nobody reading them. Labels are bounded: type is a known
// event type or "other", shard is 0 to 15.
package stats

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"meko/event-consumers/datapack-metrics-consumer/internal/contract"
)

// OtherType labels events whose type is not in contract.KnownEventTypes.
const OtherType = "other"

// Registry is what /metrics serves.
var Registry = prometheus.NewRegistry()

// The metrics, created in init so every label series exists at zero from the start.
var (
	Processed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "meko_events_consumer_events_processed_total",
		Help: "Events whose handler ran and whose offset was advanced."}, []string{"type"})
	DeadLetters = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "meko_events_consumer_dead_letters_total",
		Help: "Events moved to event_dead_letters."}, []string{"type"})
	BatchSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "meko_events_consumer_batch_seconds",
		Help:    "Time to read, handle and commit one batch.",
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30}})
	LeaseTakeovers = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "meko_events_consumer_lease_takeovers_total",
		Help: "Shards taken from a holder whose lease had expired."})
	OffsetConflicts = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "meko_events_consumer_offset_conflicts_total",
		Help: "Batches rolled back because another instance advanced the offset first."})
	LateEvents = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "meko_events_consumer_late_events_total",
		Help: "Events applied by the look-back because they committed after the offset had passed them. " +
			"Any increase means commits are stalling longer than the safety lag."})
	Unrecovered = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meko_events_consumer_unrecovered_events",
		Help: "Events found below the look-back window with no applied record: they committed later than the " +
			"window and are lost. Per shard, as of its last look-back; alert on the max over an hour."},
		[]string{"shard"})
	Errors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "meko_events_consumer_errors_total",
		Help: "Loop errors by kind."}, []string{"kind"})
	ShardsHeld = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "meko_events_consumer_shards_held",
		Help: "Shards this instance holds a lease on."})
	Lag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "meko_events_consumer_lag_seconds",
		Help: "Age of the oldest event a shard has not consumed; 0 when caught up."}, []string{"shard"})
	RunwayMonths = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "meko_events_partition_runway_months",
		Help: "Months from now to the end of the last events partition."})
	DefaultRows = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "meko_events_default_rows",
		Help: "Rows in events_default (capped at 1000); any row means a missing partition."})
)

func init() {
	Registry.MustRegister(collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		Processed, DeadLetters, BatchSeconds, LeaseTakeovers, OffsetConflicts, LateEvents, Unrecovered, Errors,
		ShardsHeld, Lag, RunwayMonths, DefaultRows)
	for _, t := range append([]string{OtherType}, contract.KnownEventTypes...) {
		Processed.WithLabelValues(t)
		DeadLetters.WithLabelValues(t)
	}
	for _, kind := range []string{"transient", "poison_paused", "unexpected", "janitor", "lookback", "no_handler"} {
		Errors.WithLabelValues(kind)
	}
	for s := range contract.ShardCount {
		Lag.WithLabelValues(strconv.Itoa(s))
		Unrecovered.WithLabelValues(strconv.Itoa(s))
	}
}

// TypeLabel bounds an event type to a label value.
func TypeLabel(eventType string) string {
	if contract.IsKnownType(eventType) {
		return eventType
	}
	return OtherType
}
