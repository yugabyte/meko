package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Browser sessions for remote access with Meko Cloud sign-in ([web] signin = "cloud", docs/web.md):
// rows of auth_sessions, keyed by the SHA-256 of the cookie a browser holds, never the cookie itself.

// CreateSession keeps a new session that lasts until expires, and drops the expired ones.
func (s *Store) CreateSession(ctx context.Context, tokenSHA256 string, expires time.Time, userAgent, ip string) error {
	now := time.Now().UTC()
	if _, err := s.w.ExecContext(ctx, "DELETE FROM auth_sessions WHERE expires_at <= ?",
		now.Format(timeLayout)); err != nil {
		return err
	}
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO auth_sessions(token_sha256, created_at, expires_at, user_agent, ip) VALUES (?, ?, ?, ?, ?)",
		tokenSHA256, now.Format(timeLayout), expires.UTC().Format(timeLayout), userAgent, ip)
	return err
}

// SessionValid reports whether a session with this cookie's SHA-256 exists and hasn't expired at now.
func (s *Store) SessionValid(ctx context.Context, tokenSHA256 string, now time.Time) (bool, error) {
	var one int
	err := s.r.QueryRowContext(ctx,
		"SELECT 1 FROM auth_sessions WHERE token_sha256 = ? AND expires_at > ?",
		tokenSHA256, now.UTC().Format(timeLayout)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// DeleteSession ends one session (signing out). Ending one that doesn't exist is no error.
func (s *Store) DeleteSession(ctx context.Context, tokenSHA256 string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM auth_sessions WHERE token_sha256 = ?", tokenSHA256)
	return err
}

// DeleteAllSessions ends every session: the Meko Cloud link changed, so who the owner is may have.
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM auth_sessions")
	return err
}
