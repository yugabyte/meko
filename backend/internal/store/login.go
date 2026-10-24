package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Owner is the one login of the web listener with remote access.
type Owner struct {
	Username    string
	PWScrypt    string
	TOTPSecret  string // "" when two-step sign-in is off
	TOTPPending string // a secret being enrolled, or ""
}

// Owner reads the owner; nil when no login is set up.
func (s *Store) Owner(ctx context.Context) (*Owner, error) {
	var o Owner
	var secret, pending sql.NullString
	err := s.r.QueryRowContext(ctx, "SELECT username, pw_scrypt, totp_secret, totp_pending FROM owner WHERE id = 1").
		Scan(&o.Username, &o.PWScrypt, &secret, &pending)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.TOTPSecret, o.TOTPPending = secret.String, pending.String
	return &o, nil
}

// SetOwner creates the owner, or replaces its login (two-step sign-in off), and signs every browser out.
func (s *Store) SetOwner(ctx context.Context, username, pwScrypt string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO owner(id, username, pw_scrypt) VALUES (1, ?, ?)
			ON CONFLICT(id) DO UPDATE SET username = excluded.username, pw_scrypt = excluded.pw_scrypt,
			  totp_secret = NULL, totp_pending = NULL`, username, pwScrypt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM auth_sessions")
		return err
	})
}

// ImportOwner adds an owner from elsewhere (AgentK's), hash and two-step secret as they are, when
// there's none yet. It reports whether it added it.
func (s *Store) ImportOwner(ctx context.Context, o Owner) (bool, error) {
	res, err := s.w.ExecContext(ctx,
		"INSERT OR IGNORE INTO owner(id, username, pw_scrypt, totp_secret) VALUES (1, ?, ?, ?)",
		o.Username, o.PWScrypt, nullIfEmpty(o.TOTPSecret))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetPassword replaces the owner's password hash and signs out every browser but the one whose
// cookie hashes to keepSession.
func (s *Store) SetPassword(ctx context.Context, pwScrypt, keepSession string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE owner SET pw_scrypt = ? WHERE id = 1", pwScrypt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM auth_sessions WHERE token_sha256 <> ?", keepSession)
		return err
	})
}

// SetTOTPPending keeps a secret being enrolled until it's confirmed.
func (s *Store) SetTOTPPending(ctx context.Context, secret string) error {
	_, err := s.w.ExecContext(ctx, "UPDATE owner SET totp_pending = ? WHERE id = 1", secret)
	return err
}

// ConfirmTOTP turns two-step sign-in on with the pending secret.
func (s *Store) ConfirmTOTP(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, "UPDATE owner SET totp_secret = totp_pending, totp_pending = NULL WHERE id = 1")
	return err
}

// DisableTOTP turns two-step sign-in off.
func (s *Store) DisableTOTP(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, "UPDATE owner SET totp_secret = NULL, totp_pending = NULL WHERE id = 1")
	return err
}

// CreateAuthSession records a signed-in browser by its cookie's hash, and drops expired ones.
func (s *Store) CreateAuthSession(ctx context.Context, tokenSHA256, userAgent, ip string,
	now, expires time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions(token_sha256, created_at, expires_at, user_agent, ip)
			VALUES (?, ?, ?, ?, ?)`, tokenSHA256, now.UTC().Format(timeLayout), expires.UTC().Format(timeLayout),
			userAgent, ip); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM auth_sessions WHERE expires_at < ?", now.UTC().Format(timeLayout))
		return err
	})
}

// AuthSessionExpiry is when a session expires; ok is false when there's no such session.
func (s *Store) AuthSessionExpiry(ctx context.Context, tokenSHA256 string) (time.Time, bool, error) {
	var exp string
	err := s.r.QueryRowContext(ctx, "SELECT expires_at FROM auth_sessions WHERE token_sha256 = ?", tokenSHA256).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(timeLayout, exp)
	return t, err == nil, err
}

// RenewAuthSession moves a session's expiry.
func (s *Store) RenewAuthSession(ctx context.Context, tokenSHA256 string, expires time.Time) error {
	_, err := s.w.ExecContext(ctx, "UPDATE auth_sessions SET expires_at = ? WHERE token_sha256 = ?",
		expires.UTC().Format(timeLayout), tokenSHA256)
	return err
}

// DeleteAuthSession signs one browser out.
func (s *Store) DeleteAuthSession(ctx context.Context, tokenSHA256 string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM auth_sessions WHERE token_sha256 = ?", tokenSHA256)
	return err
}

// DeleteSetting removes a setting; a missing one is fine.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	return err
}

func (s *Store) inTx(ctx context.Context, f func(*sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := f(tx); err != nil {
		_ = tx.Rollback() // already failing
		return err
	}
	return tx.Commit()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
