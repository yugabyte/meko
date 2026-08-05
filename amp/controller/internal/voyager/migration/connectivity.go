package migration

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConnectivityResult reports whether the source database is reachable. A failed
// probe is a successful result with Reachable=false and a populated Error, never
// an error return, so callers can surface it as HTTP 200.
type ConnectivityResult struct {
	Reachable     bool    `json:"reachable"`
	ServerVersion *string `json:"server_version,omitempty"`
	SSL           *bool   `json:"ssl,omitempty"`
	LatencyMS     *int64  `json:"latency_ms,omitempty"`
	Error         *string `json:"error,omitempty"`
}

// CheckConnectivity dials the source database directly with pgx and reads its
// version and SSL state. It never persists credentials and redacts the password
// from any error message before returning it.
func CheckConnectivity(ctx context.Context, source SourceConfig) ConnectivityResult {
	if source.Port < 1 || source.Port > 65535 {
		return failedConnectivity(source, fmt.Errorf("port %d is outside 1..65535", source.Port))
	}

	started := time.Now()
	// Build the DSN from the source first, then parse. Mutating cfg.Host/Port
	// after ParseConfig only touches the primary config: under the default
	// sslmode=prefer, ParseConfig also emits a plaintext Fallback frozen at the
	// original address, so an SSL-disabled source would retry against localhost
	// (the controller's own database) instead of the source. Parsing the real
	// DSN keeps the primary and its fallback pointed at the source.
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		url.QueryEscape(source.Username),
		url.QueryEscape(source.Password),
		source.Host,
		source.Port,
		url.PathEscape(source.Database),
	)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return failedConnectivity(source, err)
	}
	cfg.ConnectTimeout = 10 * time.Second

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return failedConnectivity(source, err)
	}
	defer conn.Close(context.Background())

	var (
		version string
		ssl     bool
	)
	err = conn.QueryRow(ctx,
		`SELECT current_setting('server_version'),
		        COALESCE((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)`).
		Scan(&version, &ssl)
	if err != nil {
		return failedConnectivity(source, err)
	}

	latency := time.Since(started).Milliseconds()
	return ConnectivityResult{
		Reachable:     true,
		ServerVersion: &version,
		SSL:           &ssl,
		LatencyMS:     &latency,
	}
}

// failedConnectivity builds a Reachable=false result, redacting the source
// password from the error message so it never leaks into responses or logs.
func failedConnectivity(source SourceConfig, err error) ConnectivityResult {
	message := err.Error()
	if source.Password != "" {
		message = strings.ReplaceAll(message, source.Password, "[REDACTED]")
		// The DSN carries the URL-escaped password, so redact that form too in
		// case it survives into a ParseConfig error.
		if esc := url.QueryEscape(source.Password); esc != source.Password {
			message = strings.ReplaceAll(message, esc, "[REDACTED]")
		}
	}
	message = fmt.Sprintf("source database is not reachable: %s", message)
	return ConnectivityResult{Reachable: false, Error: &message}
}
