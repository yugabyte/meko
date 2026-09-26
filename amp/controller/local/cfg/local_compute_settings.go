package cfg

// PostgresqlConfParams is the input to LocalComputeSettings: what a local
// endpoint's environment says about the compute the supervisor is about to
// render for.
type PostgresqlConfParams struct {
	// SharedBuffers overrides defaultLocalSharedBuffers (`--shared-buffers`).
	SharedBuffers *string
	// ListenAddr is the compute's bind IP (pg_address.ip()).
	ListenAddr string
	// UseLocalTLS: when false (the default), walproposer is told
	// sslmode=disable via yb_amp.safekeeper_conninfo_options.
	UseLocalTLS bool
	// LfcSize, when set (`--lfc-size`), enables LFC by pinning both the hard
	// cap and the soft cap to the same value.
	LfcSize *string
	// SafekeeperComputePorts are the co-located safekeepers' compute ports
	// (get_compute_port). When non-empty, walproposer is wired up
	// (synchronous_standby_names=walproposer + yb_amp.safekeepers). When
	// empty, the no-safekeeper test fallback is used
	// (synchronous_commit=remote_write + synchronous_standby_names=pageserver).
	SafekeeperComputePorts []uint16
}

// Sizing defaults for a local (`yb_amp_local`) compute. The deployed endpoint
// derives these from the pod's cgroup limits and data volume (see
// compute/endpoint_supervisor); a local run has no such reservation to read,
// so it takes fixed values instead. They are deliberately modest but not
// degenerate: the previous 1MB shared_buffers with the LFC off made local
// behaviour unrepresentative of a deployed endpoint, where both caches matter.
// `--shared-buffers` / `--lfc-size` still override; `--lfc-size 0` disables the
// cache.
const (
	defaultLocalSharedBuffers = "100MB"
	defaultLocalLfcSize       = "1GB"
)

// LocalComputeSettings is what a local compute needs that the supervisor cannot
// work out for itself, as the flat map `COMPUTE_EXTRA_SETTINGS_JSON` carries.
//
// The supervisor renders the compute spec on every deploy path, so the settings
// that are invariants of a yb-amp compute, or derived from the endpoint's
// resources, or read from `amp_registry`, are all its to decide — this is only
// the remainder. It layers after the derived settings and before the registry, so
// these override a derived default and an operator's `pg_settings` still
// overrides these.
//
// Two of them are deliberately not what a deployed endpoint would use, and are
// here rather than in the shared defaults for that reason: a local run is several
// endpoints on one developer machine, not one endpoint with a reservation.
func LocalComputeSettings(p PostgresqlConfParams) map[string]string {
	out := map[string]string{
		// Loopback, not the pod's 0.0.0.0: a local run should not publish a
		// developer's compute on every interface of a shared machine.
		"listen_addresses": p.ListenAddr,
		"hot_standby":      "on",
		"wal_keep_size":    "0",
		// 2 rather than the Postgres default, because the integration suite does
		// a lot of concurrent work on underpowered machines.
		"effective_io_concurrency": "2",
		// 5s rather than the 60s default so a wedged replication link fails a
		// test fast. Only real walsenders read it; walproposer does not.
		"wal_sender_timeout": "5s",
		// Tighter than a deployed endpoint's 500MB so any test writing more than
		// this runs through the throttle, and a lagging pageserver shows up as a
		// slow test. A perf run wanting deployed behaviour sets 500MB through
		// the endpoint's pg_settings.
		"max_replication_write_lag": "15MB",
	}

	// A local run has no pod reservation for the supervisor to size from, so
	// these are fixed instead of derived. Modest but not degenerate: both caches
	// have to matter for local behaviour to resemble a deployed endpoint's.
	sharedBuffers := defaultLocalSharedBuffers
	if p.SharedBuffers != nil {
		sharedBuffers = *p.SharedBuffers
	}
	out["shared_buffers"] = sharedBuffers

	lfcSize := defaultLocalLfcSize
	if p.LfcSize != nil {
		lfcSize = *p.LfcSize
	}
	if lfcSize != "" && lfcSize != "0" {
		out["yb_amp.max_file_cache_size"] = lfcSize
		out["yb_amp.file_cache_size_limit"] = lfcSize
	}

	if !p.UseLocalTLS {
		out["yb_amp.safekeeper_conninfo_options"] = "sslmode=disable"
	}

	// yb_amp.safekeepers is deliberately not set here. The supervisor renders it
	// once, from SAFEKEEPER_COMPUTE_PORT; a second occurrence would be taken by
	// Postgres (last wins) but not by compute_ctl's ParsedSpec, which reads the
	// first through GenericOptionsSearch::find — so the walproposer and the
	// safekeeper-sync check would target different addresses.
	if len(p.SafekeeperComputePorts) == 0 {
		// The no-safekeeper test fallback: without walproposer there is nobody to
		// wait for, so commit is acknowledged at the pageserver instead.
		out["synchronous_commit"] = "remote_write"
		out["synchronous_standby_names"] = "pageserver"
	}
	return out
}
