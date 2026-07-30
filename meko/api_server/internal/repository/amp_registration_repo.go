package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AMPRegistrationRepo reads/writes the singleton meko_system.amp_registration
// row — the AMP controller endpoint + credentials an admin registers for this
// Meko deployment.
type AMPRegistrationRepo struct {
	db *sql.DB
}

func NewAMPRegistrationRepo(db *sql.DB) *AMPRegistrationRepo {
	return &AMPRegistrationRepo{db: db}
}

// AMPRegistration is the registered AMP controller connection.
type AMPRegistration struct {
	Name      string
	BaseURL   string
	Username  string
	Password  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Get returns the registration, or (nil, nil) if AMP has not been registered.
func (r *AMPRegistrationRepo) Get(ctx context.Context) (*AMPRegistration, error) {
	var reg AMPRegistration
	err := r.db.QueryRowContext(ctx,
		`SELECT name, base_url, username, password, created_at, updated_at
		 FROM   meko_system.amp_registration
		 WHERE  singleton = TRUE`,
	).Scan(&reg.Name, &reg.BaseURL, &reg.Username, &reg.Password, &reg.CreatedAt, &reg.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("amp_registration: get: %w", err)
	}
	return &reg, nil
}

// Upsert registers (or replaces) the singleton AMP controller row. Called by
// the admin API on behalf of meko_admin, so the write goes through the same
// system-DB connection api_server uses everywhere else.
func (r *AMPRegistrationRepo) Upsert(ctx context.Context, reg AMPRegistration) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO meko_system.amp_registration (singleton, name, base_url, username, password)
		 VALUES (TRUE, $1, $2, $3, $4)
		 ON CONFLICT (singleton) DO UPDATE
		   SET name = EXCLUDED.name, base_url = EXCLUDED.base_url,
		       username = EXCLUDED.username, password = EXCLUDED.password, updated_at = now()`,
		reg.Name, reg.BaseURL, reg.Username, reg.Password,
	)
	if err != nil {
		return fmt.Errorf("amp_registration: upsert: %w", err)
	}
	return nil
}

// Delete removes the singleton registration (unregister AMP). Idempotent.
func (r *AMPRegistrationRepo) Delete(ctx context.Context) error {
	if _, err := r.db.ExecContext(ctx,
		`DELETE FROM meko_system.amp_registration WHERE singleton = TRUE`,
	); err != nil {
		return fmt.Errorf("amp_registration: delete: %w", err)
	}
	return nil
}
