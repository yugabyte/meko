// Package config is the Go port of the yb-amp launcher's on-disk config
// model and environment helpers, originally Rust
// `control_plane/src/local_env.rs`. It owns the `.yb_amp/config` (TOML)
// schema, the primitive id/auth types shared across the launcher, the
// repo-layout path helpers, and the ed25519 keypair + JWT signing used
// to mint storage-daemon tokens.
//
// Scope is the minimal yb-amp port: external YugabyteDB always
// (controller.yb is set), Trust auth for the storage daemons, no local
// TLS, a single pageserver. TLS cert generation and the local-Postgres
// controller behavior are intentionally omitted; the struct fields for
// those are still modeled so configs round-trip byte-compatibly.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// BranchID is a 32-hex-char branch identifier (Rust `utils::id::BranchId`,
// a 128-bit id that (de)serializes as a hex string in human-readable
// formats such as TOML).
type BranchID = string

// NodeID is a storage-node id (Rust `utils::id::NodeId`, a `u64` newtype
// serialized transparently as a bare integer). Modeled as int64 here; the
// launcher never uses ids beyond the int64 range.
type NodeID = int64

// Lsn is a Postgres log sequence number in the "X/Y" upper/lower hex form
// (Rust `utils::lsn::Lsn`).
type Lsn = string

// AuthType mirrors Rust `postgres_backend::AuthType`. Its serde repr is the
// bare variant name, so the wire strings are exactly "Trust" and
// "YbAmpJWT". Only these two variants matter for the minimal port.
type AuthType string

const (
	// AuthTrust is passwordless trust auth (the storage-daemon default in
	// the minimal port).
	AuthTrust AuthType = "Trust"
	// AuthYbAmpJWT expects a JWT in place of a cleartext password.
	AuthYbAmpJWT AuthType = "YbAmpJWT"
)

// DefaultPgVersion is the default major Postgres version (Rust
// `DEFAULT_PG_VERSION`).
const DefaultPgVersion = 18

// defaultYbPort is the YSQL port applied to a host_ports entry that omits
// ":port" (Rust `ysql_client::DEFAULT_YB_PORT`).
const defaultYbPort = 5433

// hostPort is one parsed YB tserver endpoint.
type hostPort struct {
	host string
	port uint16
}

// parseHostPorts parses the CSV wire form `host:port[,host:port,...]`
// (Rust `ysql_client::HostPorts::from_str`). A missing ":port" defaults to
// defaultYbPort. At least one endpoint is required.
func parseHostPorts(s string) ([]hostPort, error) {
	var out []hostPort
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if idx := strings.LastIndex(entry, ":"); idx >= 0 {
			host := entry[:idx]
			portStr := entry[idx+1:]
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				return nil, fmt.Errorf("invalid port in YB endpoint %q", entry)
			}
			if host == "" {
				return nil, fmt.Errorf("empty host in YB endpoint %q", entry)
			}
			out = append(out, hostPort{host: host, port: uint16(port)})
		} else {
			out = append(out, hostPort{host: entry, port: defaultYbPort})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("host_ports must contain at least one host:port (got %q)", s)
	}
	return out, nil
}

// YbConnConfig mirrors Rust `ysql_client::YbConnConfig`: the single
// canonical YugabyteDB connection block (`[*.yb]`) every yb-amp daemon
// deserializes. Connection only — no table/schema; each component
// hardcodes the table(s) it owns.
//
// The Rust type is `deny_unknown_fields`; all its fields are modeled here
// so the TOML round-trips faithfully. HostPorts is stored as its CSV wire
// form (Rust parses it into a list via a custom serde impl).
type YbConnConfig struct {
	// HostPorts is the CSV wire form `host:port[,host:port,...]` (port
	// defaults to 5433 when omitted).
	HostPorts string `toml:"host_ports"`
	// User defaults to "yugabyte" (Rust `default_yb_user`).
	User string `toml:"user"`
	// Dbname defaults to "yb_storage" (Rust `default_yb_dbname`).
	Dbname string `toml:"dbname"`
	// PasswordEnv names an environment variable holding the password.
	PasswordEnv *string `toml:"password_env,omitempty"`
	// Sslmode: nil or "disable" -> NoTls; any other value -> TLS.
	Sslmode *string `toml:"sslmode,omitempty"`
	// Sslrootcert is a PEM CA bundle path for strict TLS verification.
	Sslrootcert *string `toml:"sslrootcert,omitempty"`
	// DisableTransactionalWrites toggles the fast-path-upsert GUC.
	DisableTransactionalWrites bool `toml:"disable_transactional_writes,omitempty"`
	// HotPath is the hot-path transport: "tsclient" (default, omitted on
	// the wire) or "ysql". Modeled as a pointer so the default stays
	// omitted, matching Rust's `skip_serializing_if = HotPath::is_default`.
	HotPath *string `toml:"hot_path,omitempty"`
	// TserverRPCAddrs overrides the tsclient RPC endpoints (CSV).
	TserverRPCAddrs *string `toml:"tserver_rpc_addrs,omitempty"`
}

// effectiveUser / effectiveDbname apply the serde defaults without
// mutating the struct (so serialized output stays faithful).
func (c *YbConnConfig) effectiveUser() string {
	if c.User == "" {
		return "yugabyte"
	}
	return c.User
}

func (c *YbConnConfig) effectiveDbname() string {
	if c.Dbname == "" {
		return "yb_storage"
	}
	return c.Dbname
}

// ToDatabaseURL renders this connection config as a libpq/diesel
// `postgresql://` URL, matching Rust `YbConnConfig::to_database_url`. All
// host:port pairs are emitted as a libpq multi-host authority;
// `password_env` is resolved from the environment and percent-encoded.
func (c *YbConnConfig) ToDatabaseURL() (string, error) {
	var sb strings.Builder
	sb.WriteString("postgresql://")
	sb.WriteString(percentEncode(c.effectiveUser()))
	if c.PasswordEnv != nil {
		pw, ok := os.LookupEnv(*c.PasswordEnv)
		if !ok {
			return "", fmt.Errorf("password env var `%s` is not set", *c.PasswordEnv)
		}
		sb.WriteByte(':')
		sb.WriteString(percentEncode(pw))
	}
	sb.WriteByte('@')
	hps, err := parseHostPorts(c.HostPorts)
	if err != nil {
		return "", err
	}
	hosts := make([]string, len(hps))
	for i, hp := range hps {
		hosts[i] = fmt.Sprintf("%s:%d", hp.host, hp.port)
	}
	sb.WriteString(strings.Join(hosts, ","))
	sb.WriteByte('/')
	sb.WriteString(percentEncode(c.effectiveDbname()))
	var params []string
	if c.Sslmode != nil {
		params = append(params, "sslmode="+percentEncode(*c.Sslmode))
	}
	if c.Sslrootcert != nil {
		params = append(params, "sslrootcert="+percentEncode(*c.Sslrootcert))
	}
	if len(params) > 0 {
		sb.WriteByte('?')
		sb.WriteString(strings.Join(params, "&"))
	}
	return sb.String(), nil
}

// percentEncode escapes everything outside the RFC 3986 unreserved set
// (ALPHA / DIGIT / - . _ ~), matching Rust `ysql_client::percent_encode`.
func percentEncode(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		b := s[i]
		unreserved := (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
			(b >= '0' && b <= '9') || b == '-' || b == '.' || b == '_' || b == '~'
		if unreserved {
			sb.WriteByte(b)
		} else {
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}

// HumantimeDuration is a time.Duration that (de)serializes as a humantime
// string (e.g. "10s", "1s", "1h 30m"), matching the Rust
// `#[serde(with = "humantime_serde")]` durations in YbAmpControllerConf.
type HumantimeDuration time.Duration

// MarshalText renders the duration in humantime's canonical form. This is
// a faithful port of humantime's `FormattedDuration` Display impl.
func (d HumantimeDuration) MarshalText() ([]byte, error) {
	return []byte(formatHumantime(time.Duration(d))), nil
}

// UnmarshalText parses a humantime duration string.
func (d *HumantimeDuration) UnmarshalText(text []byte) error {
	dur, err := parseHumantime(string(text))
	if err != nil {
		return err
	}
	*d = HumantimeDuration(dur)
	return nil
}

// Duration returns the underlying time.Duration.
func (d HumantimeDuration) Duration() time.Duration {
	return time.Duration(d)
}

// formatHumantime mirrors humantime v2 `FormattedDuration::fmt`: it breaks
// the duration into years/months/days/h/m/s/ms/us/ns (years=365.25d,
// months=30.44d) and joins the non-zero components with spaces. Larger
// units (year/month/day) pluralize; the rest use fixed suffixes.
func formatHumantime(d time.Duration) string {
	if d < 0 {
		// humantime durations are unsigned; the launcher never uses
		// negative timeouts. Clamp to keep the formatter total.
		d = 0
	}
	totalNanos := uint64(d.Nanoseconds())
	secs := totalNanos / 1_000_000_000
	nanos := uint32(totalNanos % 1_000_000_000)

	if secs == 0 && nanos == 0 {
		return "0s"
	}

	years := secs / 31_557_600
	ydays := secs % 31_557_600
	months := ydays / 2_630_016
	mdays := ydays % 2_630_016
	days := mdays / 86400
	daySecs := mdays % 86400
	hours := daySecs / 3600
	minutes := daySecs % 3600 / 60
	seconds := daySecs % 60

	millis := nanos / 1_000_000
	micros := nanos / 1000 % 1000
	nanosec := nanos % 1000

	var sb strings.Builder
	started := false
	itemPlural(&sb, &started, "year", years)
	itemPlural(&sb, &started, "month", months)
	itemPlural(&sb, &started, "day", days)
	item(&sb, &started, "h", hours)
	item(&sb, &started, "m", minutes)
	item(&sb, &started, "s", seconds)
	item(&sb, &started, "ms", uint64(millis))
	item(&sb, &started, "us", uint64(micros))
	item(&sb, &started, "ns", uint64(nanosec))
	return sb.String()
}

func item(sb *strings.Builder, started *bool, name string, value uint64) {
	if value > 0 {
		if *started {
			sb.WriteByte(' ')
		}
		sb.WriteString(strconv.FormatUint(value, 10))
		sb.WriteString(name)
		*started = true
	}
}

func itemPlural(sb *strings.Builder, started *bool, name string, value uint64) {
	if value > 0 {
		if *started {
			sb.WriteByte(' ')
		}
		sb.WriteString(strconv.FormatUint(value, 10))
		sb.WriteString(name)
		if value != 1 {
			sb.WriteByte('s')
		}
		*started = true
	}
}

// humantimeUnits maps humantime unit spellings to their duration, matching
// humantime v2 `Unit::from_str`.
var humantimeUnits = map[string]time.Duration{
	"nanos": time.Nanosecond, "nsec": time.Nanosecond, "ns": time.Nanosecond,
	"usec": time.Microsecond, "us": time.Microsecond, "µs": time.Microsecond,
	"millis": time.Millisecond, "msec": time.Millisecond, "ms": time.Millisecond,
	"seconds": time.Second, "second": time.Second, "secs": time.Second, "sec": time.Second, "s": time.Second,
	"minutes": time.Minute, "minute": time.Minute, "min": time.Minute, "mins": time.Minute, "m": time.Minute,
	"hours": time.Hour, "hour": time.Hour, "hr": time.Hour, "hrs": time.Hour, "h": time.Hour,
	"days": 86400 * time.Second, "day": 86400 * time.Second, "d": 86400 * time.Second,
	"weeks": 7 * 86400 * time.Second, "week": 7 * 86400 * time.Second, "wk": 7 * 86400 * time.Second, "wks": 7 * 86400 * time.Second, "w": 7 * 86400 * time.Second,
	"months": 2_630_016 * time.Second, "month": 2_630_016 * time.Second, "M": 2_630_016 * time.Second,
	"years": 31_557_600 * time.Second, "year": 31_557_600 * time.Second, "yr": 31_557_600 * time.Second, "yrs": 31_557_600 * time.Second, "y": 31_557_600 * time.Second,
}

// parseHumantime parses a humantime duration string such as "10s",
// "1h 30m", "1h30m" or "500ms". Whitespace between number/unit pairs is
// optional. It is the inverse of formatHumantime for the values the
// launcher uses.
func parseHumantime(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("humantime: empty duration string")
	}
	var total time.Duration
	i := 0
	n := len(s)
	sawComponent := false
	for i < n {
		// Skip whitespace between components.
		for i < n && s[i] == ' ' {
			i++
		}
		if i >= n {
			break
		}
		// Read the numeric part.
		start := i
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == start {
			return 0, fmt.Errorf("humantime: expected number in %q at position %d", s, i)
		}
		value, err := strconv.ParseUint(s[start:i], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("humantime: invalid number in %q: %w", s, err)
		}
		// Skip optional whitespace between the number and its unit.
		for i < n && s[i] == ' ' {
			i++
		}
		// Read the unit (letters, plus µ for µs).
		unitStart := i
		for i < n && s[i] != ' ' && !(s[i] >= '0' && s[i] <= '9') {
			i++
		}
		unit := s[unitStart:i]
		if unit == "" {
			return 0, fmt.Errorf("humantime: missing unit after %d in %q", value, s)
		}
		perUnit, ok := humantimeUnits[unit]
		if !ok {
			return 0, fmt.Errorf("humantime: unknown unit %q in %q", unit, s)
		}
		total += time.Duration(value) * perUnit
		sawComponent = true
	}
	if !sawComponent {
		return 0, fmt.Errorf("humantime: no duration components in %q", s)
	}
	return total, nil
}
