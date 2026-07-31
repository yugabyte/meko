package cfg

import (
	"fmt"
	"regexp"
	"strings"
)

// unquotedGUCRe matches values that need no quoting in postgresql.conf. A
// verbatim port of the Rust `UNQUOTED_RE`
// (control_plane/src/postgresql_conf.rs): the first alternative is the
// INTEGER rule ("123", "-10kB"), the second the single-word UNQUOTED_STRING
// rule ("off", "posix"). Everything else is quoted.
var unquotedGUCRe = regexp.MustCompile(`(^[-+]?[0-9]+[a-zA-Z]*$)|(^[a-zA-Z][a-zA-Z0-9]*$)`)

// escapeGUC escapes a value for postgresql.conf. Port of Rust `escape_str`.
func escapeGUC(s string) string {
	if unquotedGUCRe.MatchString(s) {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, "\n", `\n`, `'`, `''`)
	return "'" + r.Replace(s) + "'"
}

// postgresConf is the in-memory postgresql.conf builder, mirroring Rust
// `PostgresConf`. append() adds `key=escape(value)\n`; appendLine() adds a
// raw line verbatim.
type postgresConf struct {
	b strings.Builder
}

func (c *postgresConf) append(option, value string) {
	fmt.Fprintf(&c.b, "%s=%s\n", option, escapeGUC(value))
}

func (c *postgresConf) appendLine(line string) {
	c.b.WriteString(line)
}

func (c *postgresConf) String() string { return c.b.String() }

// PostgresqlConfParams is the input to BuildPostgresqlConf — the fields Rust
// `Endpoint::setup_pg_conf` reads (control_plane/src/endpoint.rs ~464-610),
// restricted to the Primary-mode path the harness uses.
type PostgresqlConfParams struct {
	// SharedBuffers overrides the default "1MB" (`--shared-buffers`).
	SharedBuffers *string
	// ListenAddr is the compute's bind IP (pg_address.ip()).
	ListenAddr string
	// Port is the compute's Postgres port (pg_address.port()).
	Port uint16
	// UseLocalTLS: when false (the default), walproposer is told
	// sslmode=disable via yb_amp.safekeeper_conninfo_options.
	UseLocalTLS bool
	// LfcSize, when set (`--lfc-size`), enables LFC by pinning both the hard
	// cap and the soft cap to the same value.
	LfcSize *string
	// PgSettings are user `--pg-setting` overrides, appended last so they
	// win over the defaults.
	PgSettings [][2]string

	// SafekeeperComputePorts are the co-located safekeepers' compute ports
	// (get_compute_port). When non-empty, walproposer is wired up
	// (synchronous_standby_names=walproposer + yb_amp.safekeepers). When
	// empty, the no-safekeeper test fallback is used
	// (synchronous_commit=remote_write + synchronous_standby_names=pageserver).
	SafekeeperComputePorts []uint16
}

// BuildPostgresqlConf builds the postgresql.conf string for a Primary
// compute. Port of Rust `Endpoint::setup_pg_conf` (ComputeMode::Primary
// arm). The Static/Replica arms are intentionally omitted (out of scope).
func BuildPostgresqlConf(p PostgresqlConfParams) string {
	c := &postgresConf{}
	c.append("max_wal_senders", "10")
	c.append("wal_log_hints", "off")
	c.append("max_replication_slots", "10")
	c.append("hot_standby", "on")
	sharedBuffers := "1MB"
	if p.SharedBuffers != nil {
		sharedBuffers = *p.SharedBuffers
	}
	c.append("shared_buffers", sharedBuffers)
	c.append("effective_io_concurrency", "2")
	c.append("fsync", "off")
	c.append("full_page_writes", "off")
	c.append("max_connections", "100")
	c.append("wal_level", "logical")
	c.append("wal_sender_timeout", "5s")
	c.append("listen_addresses", p.ListenAddr)
	c.append("port", fmt.Sprintf("%d", p.Port))
	c.append("wal_keep_size", "0")
	c.append("restart_after_crash", "off")
	c.append("shared_preload_libraries", "yb_amp")

	if !p.UseLocalTLS {
		c.append("yb_amp.safekeeper_conninfo_options", "sslmode=disable")
	}

	if p.LfcSize != nil {
		c.append("yb_amp.max_file_cache_size", *p.LfcSize)
		c.append("yb_amp.file_cache_size_limit", *p.LfcSize)
	}

	for _, kv := range p.PgSettings {
		c.append(kv[0], kv[1])
	}

	c.appendLine("")

	// ComputeMode::Primary
	c.append("max_replication_write_lag", "15MB")
	c.append("max_replication_flush_lag", "10GB")
	if len(p.SafekeeperComputePorts) > 0 {
		c.append("synchronous_standby_names", "walproposer")
		hosts := make([]string, len(p.SafekeeperComputePorts))
		for i, port := range p.SafekeeperComputePorts {
			hosts[i] = fmt.Sprintf("localhost:%d", port)
		}
		c.append("yb_amp.safekeepers", strings.Join(hosts, ","))
	} else {
		c.append("synchronous_commit", "remote_write")
		c.append("synchronous_standby_names", "pageserver")
	}

	return c.String()
}
