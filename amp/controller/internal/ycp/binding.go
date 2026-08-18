package ycp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Binding is the single YCP environment this controller is bound to. It is set
// once, by the reconcile-agent after it enrolls, and never rebound to a
// different YCP — that is what stops a second agent pointed at another control
// plane from taking the controller over.
type Binding struct {
	YCPURL        string
	EnvironmentID string
}

// BindingStore persists the write-once binding in the controller's own schema
// (the ycp_binding singleton from migration 0007), over the same pool as the
// task log.
type BindingStore struct {
	pool *pgxpool.Pool
}

// NewBindingStore constructs the store over the controller pool.
func NewBindingStore(pool *pgxpool.Pool) *BindingStore { return &BindingStore{pool: pool} }

// Load returns the current binding, or (nil, nil) when the controller is not yet
// bound.
func (s *BindingStore) Load(ctx context.Context) (*Binding, error) {
	var b Binding
	err := s.pool.QueryRow(ctx,
		`SELECT ycp_url, environment_id FROM ycp_binding WHERE singleton = true`).
		Scan(&b.YCPURL, &b.EnvironmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load ycp binding: %w", err)
	}
	return &b, nil
}

// Bind records (ycpURL, envID) as the binding if none exists yet, and reports
// what the binding is now. created is true only when this call inserted it; on a
// controller that was already bound it returns the existing binding with
// created=false, leaving the caller to decide whether that is the same binding
// (idempotent re-bind) or a conflict.
func (s *BindingStore) Bind(ctx context.Context, ycpURL, envID string) (cur *Binding, created bool, err error) {
	var got Binding
	err = s.pool.QueryRow(ctx,
		`INSERT INTO ycp_binding (singleton, ycp_url, environment_id)
		 VALUES (true, $1, $2)
		 ON CONFLICT (singleton) DO NOTHING
		 RETURNING ycp_url, environment_id`,
		ycpURL, envID).Scan(&got.YCPURL, &got.EnvironmentID)
	if err == nil {
		return &got, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("bind ycp: %w", err)
	}
	// Conflict: a binding already existed, so the insert wrote nothing and
	// returned no row. Report the incumbent.
	existing, lerr := s.Load(ctx)
	if lerr != nil {
		return nil, false, lerr
	}
	return existing, false, nil
}
