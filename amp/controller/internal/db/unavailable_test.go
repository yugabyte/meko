package db_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

func TestUnavailable(t *testing.T) {
	cases := []struct {
		name            string
		err             error
		wantUnavailable bool
		wantSafe        bool
	}{
		{name: "nil"},
		{
			name: "dial refused",
			err:  &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")},
			// A bare dial error carries no proof of *when* it happened; only the
			// pgconn.ConnectError wrapper (see TestUnavailableRealDialFailure) says
			// the whole connection attempt failed.
			wantUnavailable: true,
		},
		{
			name:            "hostname does not resolve",
			err:             &net.DNSError{Err: "no such host", Name: "yb-tserver-0"},
			wantUnavailable: true,
		},
		{
			name:            "connection died mid-read",
			err:             io.ErrUnexpectedEOF,
			wantUnavailable: true,
		},
		{
			name:            "admin shutdown (57P01)",
			err:             &pgconn.PgError{Severity: "FATAL", Code: "57P01", Message: "terminating connection due to administrator command"},
			wantUnavailable: true,
		},
		{
			name:            "still starting up (57P03)",
			err:             &pgconn.PgError{Severity: "FATAL", Code: "57P03", Message: "the database system is starting up"},
			wantUnavailable: true,
			wantSafe:        true,
		},
		{
			name:            "connection failure (08006)",
			err:             &pgconn.PgError{Severity: "FATAL", Code: "08006", Message: "connection failure"},
			wantUnavailable: true,
		},
		{
			name:            "too many connections (53300)",
			err:             &pgconn.PgError{Severity: "FATAL", Code: "53300", Message: "too many clients already"},
			wantUnavailable: true,
			wantSafe:        true,
		},
		{
			name: "a cancelled query is not the store being down (57014)",
			err:  &pgconn.PgError{Severity: "ERROR", Code: "57014", Message: "canceling statement due to statement timeout"},
		},
		{
			name: "an idle session reaped is not the store being down (57P05)",
			err:  &pgconn.PgError{Severity: "FATAL", Code: "57P05", Message: "terminating connection due to idle-session timeout"},
		},
		{
			name: "bad password is a verdict, not an outage",
			err:  &pgconn.PgError{Severity: "FATAL", Code: "28P01", Message: "password authentication failed"},
		},
		{
			name: "unique violation is a verdict",
			err:  &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint"},
		},
		{
			name: "deadline is not the store being down",
			err:  context.DeadlineExceeded,
		},
		{name: "an ordinary controller error", err: errors.New("boom")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Wrapped, the way a store method returns it (`get cluster: %w`).
			for _, err := range []error{tc.err, wrap(tc.err)} {
				if got := db.Unavailable(err); got != tc.wantUnavailable {
					t.Errorf("Unavailable(%v) = %v, want %v", err, got, tc.wantUnavailable)
				}
				if got := db.SafeToRetry(err); got != tc.wantSafe {
					t.Errorf("SafeToRetry(%v) = %v, want %v", err, got, tc.wantSafe)
				}
			}
		})
	}
}

// TestUnavailableRealDialFailure pins the failure the controller actually
// reported as a 500 (#886): a pgx connection attempt that could not reach the
// tserver holding the state store. It is the pgconn.ConnectError wrapper — not
// the dial error inside it — that makes the request safe to replay, since it
// means no statement was ever sent.
func TestUnavailableRealDialFailure(t *testing.T) {
	// Port 1 on loopback: refused immediately, no name resolution, no waiting.
	_, err := pgconn.Connect(context.Background(), "postgres://yugabyte@127.0.0.1:1/yb_storage")
	if err == nil {
		t.Skip("something is listening on 127.0.0.1:1")
	}
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		t.Fatalf("connect error = %T (%v), want *pgconn.ConnectError", err, err)
	}
	if err := wrap(err); !db.Unavailable(err) || !db.SafeToRetry(err) {
		t.Errorf("Unavailable/SafeToRetry(%v) = %v/%v, want true/true",
			err, db.Unavailable(err), db.SafeToRetry(err))
	}
}

// TestAfterWriteIsNotSafeToRetry: past a durable write, a connection failure
// stops being proof that nothing happened. It is still the store being
// unavailable — it just cannot be replayed.
func TestAfterWriteIsNotSafeToRetry(t *testing.T) {
	_, dialErr := pgconn.Connect(context.Background(), "postgres://yugabyte@127.0.0.1:1/yb_storage")
	if dialErr == nil {
		t.Skip("something is listening on 127.0.0.1:1")
	}
	if !db.SafeToRetry(wrap(dialErr)) {
		t.Fatalf("a failed connection attempt should be safe to retry on its own")
	}
	marked := wrap(db.AfterWrite(dialErr))
	if !db.Unavailable(marked) {
		t.Errorf("Unavailable(marked) = false, want true (still an outage)")
	}
	if db.SafeToRetry(marked) {
		t.Errorf("SafeToRetry(marked) = true, want false (the write may have landed)")
	}
	if db.AfterWrite(nil) != nil {
		t.Errorf("AfterWrite(nil) should stay nil")
	}
}

func wrap(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("get cluster: %w", err)
}
