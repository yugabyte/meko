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

// Writes (register / update / delete) are owned by meko_admin, which writes
// meko_system.amp_registration directly; api_server only reads it (above).
