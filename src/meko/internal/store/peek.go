package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
)

// KeyInstallID is the setting that holds the install's random id, which call home reports.
const KeyInstallID = keyInstallID

// Peek reads the database at path without writing to it, for a command that runs beside the server
// or before it: its schema version (PRAGMA user_version) and the settings named in keys that are set.
// A missing database is version 0 with no settings, and so is one too old to have a settings table.
func Peek(ctx context.Context, path string, keys ...string) (int, map[string]string, error) {
	settings := map[string]string{}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return 0, settings, nil
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Set("_busy_timeout", "5000")
	db, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String())
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = db.Close() }()
	var v int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, nil, err
	}
	for _, k := range keys {
		var value string
		err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", k).Scan(&value)
		switch {
		case err == nil:
			settings[k] = value
		case errors.Is(err, sql.ErrNoRows):
		case v == 0:
			return v, settings, nil // no tables yet
		default:
			return v, nil, err
		}
	}
	return v, settings, nil
}
