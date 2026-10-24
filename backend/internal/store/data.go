package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Settings keys.
const (
	keyInstallID       = "install_id"
	keyDefaultDatapack = "default_datapack_id"
	keyLocalUserID     = "local_user_id"
)

// timeLayout is RFC 3339 in UTC with a fixed-width fraction, so stored times sort as text.
// (time.RFC3339Nano drops trailing zeros, and "…:03.1Z" sorts after "…:03.12Z".)
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// DefaultDatapackName names the datapack made on first start. It must not start with
// meko_default_datapack, which Meko Cloud reserves for its own default datapacks.
const DefaultDatapackName = "default"

// Setting reads a setting; ok is false when it isn't set.
func (s *Store) Setting(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.r.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.w.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}

// EnsureInstallID returns this install's id, making one the first time.
func (s *Store) EnsureInstallID(ctx context.Context) (string, error) {
	return s.ensureID(ctx, keyInstallID)
}

// EnsureLocalUserID returns the id of the install's one user, making one the first time. It's
// the user_id on every memory, as an account id is in prod.
func (s *Store) EnsureLocalUserID(ctx context.Context) (string, error) {
	return s.ensureID(ctx, keyLocalUserID)
}

func (s *Store) ensureID(ctx context.Context, key string) (string, error) {
	if id, ok, err := s.Setting(ctx, key); err != nil || ok {
		return id, err
	}
	if _, err := s.w.ExecContext(ctx,
		"INSERT OR IGNORE INTO settings(key, value) VALUES (?, ?)", key, uuid.NewString()); err != nil {
		return "", err
	}
	id, _, err := s.Setting(ctx, key)
	return id, err
}

// DatapackExists reports whether a datapack with this id exists.
func (s *Store) DatapackExists(ctx context.Context, id string) (bool, error) {
	err := datapackExists(ctx, s.r, id)
	if errors.Is(err, ErrNoDatapack) {
		return false, nil
	}
	return err == nil, err
}

// Datapack is a row of datapacks.
type Datapack struct {
	ID                       string    `json:"id"`
	Name                     string    `json:"name"`
	Description              string    `json:"description"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
	ConversationSearchOptOut bool      `json:"conversation_search_opt_out"`
}

// EnsureDefaultDatapack makes the default datapack on first start, once: if the user deletes it
// later, it isn't made again. It returns its id.
func (s *Store) EnsureDefaultDatapack(ctx context.Context) (string, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", keyDefaultDatapack).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id = uuid.NewString()
	now := time.Now().UTC().Format(timeLayout)
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO datapacks(id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		id, DefaultDatapackName, "Created by Meko on first start.", now, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES (?, ?)", keyDefaultDatapack, id); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// Datapacks lists every datapack, oldest first.
func (s *Store) Datapacks(ctx context.Context) ([]Datapack, error) {
	rows, err := s.r.QueryContext(ctx, "SELECT "+datapackCols+" FROM datapacks ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Datapack{}
	for rows.Next() {
		d, err := scanDatapack(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
