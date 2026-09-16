package db

import (
	"crypto/tls"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// installClientCertReload makes the pool read sslcert/sslkey on every TLS
// handshake instead of once at parse time, so a certificate cert-manager
// renewed in place is presented by the next connection without a restart. A
// DSN without a client certificate is left alone.
//
// The files are read at install as well, so a bad path or a key that does not
// match its certificate fails startup here, with the file named, rather than on
// the first handshake.
//
// This covers db.Connect's pool, which is the only long-lived one. Two other
// paths parse the same DSN and keep pgx's static Certificates: migrate.go's
// stdlib.OpenDB, which is short-lived, and pglease.Open, which has no caller
// today (task/locks.go uses pglease.NewStore over this pool). Either would
// need the same call before it could outlive a certificate.
func installClientCertReload(cfg *pgxpool.Config, dsn string) error {
	if cfg.ConnConfig.TLSConfig == nil {
		return nil
	}
	certPath, keyPath := dsnParam(dsn, "sslcert"), dsnParam(dsn, "sslkey")
	if certPath == "" || keyPath == "" {
		return nil
	}
	load := func() (*tls.Certificate, error) {
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("client certificate %s / %s: %w", certPath, keyPath, err)
		}
		return &cert, nil
	}
	if _, err := load(); err != nil {
		return err
	}
	reload := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return load() }
	tlsConfigs := []*tls.Config{cfg.ConnConfig.TLSConfig}
	// A multi-host DSN carries one TLS config per fallback host.
	for _, fb := range cfg.ConnConfig.Fallbacks {
		if fb.TLSConfig != nil {
			tlsConfigs = append(tlsConfigs, fb.TLSConfig)
		}
	}
	for _, tc := range tlsConfigs {
		tc.Certificates = nil
		tc.GetClientCertificate = reload
	}
	return nil
}

// dsnParam reads one connection setting the way pgx resolves it: from either
// DSN form it accepts (a postgres:// URL's query parameter, or a key=value
// string), else from the setting's PG* environment variable, the DSN winning
// when both are set.
//
// The environment fallback matters because pgx builds its TLS config from the
// same two sources. A certificate configured through PGSSLCERT would otherwise
// give a pool that presents a client certificate this file never finds, and so
// never reloads.
func dsnParam(dsn, key string) string {
	var v string
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		if u, err := url.Parse(dsn); err == nil {
			v = u.Query().Get(key)
		}
	default:
		for kv := range strings.FieldsSeq(dsn) {
			if k, val, ok := strings.Cut(kv, "="); ok && k == key {
				v = strings.Trim(val, "'")
				break
			}
		}
	}
	if v == "" {
		v = os.Getenv("PG" + strings.ToUpper(key))
	}
	return v
}
