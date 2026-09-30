package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Controller-wide state the controller writes itself, one row per key in
// amp_registry.controller_settings (migration 0009).

// fleetVersionKey holds the version the whole fleet was last confirmed at. It
// moves only when every endpoint has been seen at the new value, so after a roll
// that stops part-way it can name a version some endpoints have since left — which
// only ever lets a roll through. The refusal trusts it only at the pin, where
// EffectiveFleetVersion checks it against the endpoints (ac-endpoint-upgrade.md
// §6.1).
const fleetVersionKey = "fleet_version"

// FleetVersionMixed is the fleet version of a fleet whose endpoints were not all
// at one version when it was seeded. It never equals an endpoint version, so it
// never blocks a roll.
const FleetVersionMixed = "mixed"

// FleetVersion reads the recorded fleet version; "" when none is recorded.
func (b *PgBackend) FleetVersion(ctx context.Context) (string, error) {
	var v string
	err := b.pool.QueryRow(ctx,
		`SELECT value FROM amp_registry.controller_settings WHERE key = $1`, fleetVersionKey).Scan(&v)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("read fleet version: %w", err)
	}
	return v, nil
}

// SetFleetVersion records the fleet version (an upsert, idempotent).
func (b *PgBackend) SetFleetVersion(ctx context.Context, version string) error {
	_, err := b.pool.Exec(ctx, `
		INSERT INTO amp_registry.controller_settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		fleetVersionKey, version)
	if err != nil {
		return fmt.Errorf("set fleet version: %w", err)
	}
	return nil
}

// FleetVersion mirrors PgBackend.FleetVersion.
func (m *MemBackend) FleetVersion(_ context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fleetVersion, nil
}

// SetFleetVersion mirrors PgBackend.SetFleetVersion.
func (m *MemBackend) SetFleetVersion(_ context.Context, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fleetVersion = version
	return nil
}

// FleetVersionStore is what the fleet-version functions below read and write.
type FleetVersionStore interface {
	FleetVersion(ctx context.Context) (string, error)
	SetFleetVersion(ctx context.Context, version string) error
	AllEndpointDeploys(ctx context.Context) ([]EndpointDeploy, error)
}

// SeedFleetVersion runs at startup, with pin the version this controller deploys
// new endpoints at, which must be set.
//
// A fleet with no endpoints is at the pin whatever was recorded: a controller
// upgraded over an empty fleet has nothing to roll. A recorded value over a
// non-empty fleet is kept. With none recorded — a controller upgraded onto this
// migration with endpoints running — it is seeded from the endpoints, never from
// the pin, which would claim a roll that never happened.
func SeedFleetVersion(ctx context.Context, s FleetVersionStore, pin string) error {
	if pin == "" {
		return fmt.Errorf("pin is empty, cannot seed fleet version")
	}
	fleet, err := s.AllEndpointDeploys(ctx)
	if err != nil {
		return fmt.Errorf("enumerate endpoints: %w", err)
	}
	if len(fleet) == 0 {
		return s.SetFleetVersion(ctx, pin)
	}
	recorded, err := s.FleetVersion(ctx)
	if err != nil || recorded != "" {
		return err
	}
	return s.SetFleetVersion(ctx, CommonEndpointVersion(fleet))
}

// EffectiveFleetVersion is the fleet version as the API reports it and the roll
// refusal reads it: the recorded one, with two corrections.
//
// A fleet with no endpoints is at pin. The seed writes that too, but only at
// startup, and a rollout can start a pod of another version after this one.
//
// A record equal to pin is believed only if every endpoint is at pin, because
// that is the claim the refusal acts on and a record can reach it falsely: record
// X, upgrade to Y, create endpoints at Y, roll back to X, and the record equals
// the pin while the Y endpoints are not on it. Such a record reads "mixed", so a
// roll can move them. A record that differs from pin may lag and is left alone.
func EffectiveFleetVersion(ctx context.Context, s interface {
	FleetVersion(ctx context.Context) (string, error)
	AllEndpointDeploys(ctx context.Context) ([]EndpointDeploy, error)
}, pin string) (string, error) {
	recorded, err := s.FleetVersion(ctx)
	if err != nil || pin == "" {
		return recorded, err
	}
	fleet, err := s.AllEndpointDeploys(ctx)
	if err != nil {
		return "", fmt.Errorf("enumerate endpoints: %w", err)
	}
	switch {
	case len(fleet) == 0:
		return pin, nil
	case recorded == pin && CommonEndpointVersion(fleet) != pin:
		return FleetVersionMixed, nil
	}
	return recorded, nil
}

// AdvanceFleetVersion records version as the fleet version when a fresh read
// finds every live endpoint at it — after a roll, and after an endpoint is
// created, which is what moves a fleet that was empty. Anything short of that
// leaves the record where it was, so it never claims more than is true.
func AdvanceFleetVersion(ctx context.Context, s interface {
	SetFleetVersion(ctx context.Context, version string) error
	AllEndpointDeploys(ctx context.Context) ([]EndpointDeploy, error)
}, version string) error {
	fleet, err := s.AllEndpointDeploys(ctx)
	if err != nil {
		return fmt.Errorf("enumerate endpoints: %w", err)
	}
	if len(fleet) > 0 && CommonEndpointVersion(fleet) != version {
		return nil
	}
	return s.SetFleetVersion(ctx, version)
}

// CommonEndpointVersion is the version every endpoint in fleet records, or
// FleetVersionMixed when they differ or record none — endpoints from before
// versions were recorded, which a stored "" would read as nothing recorded at all.
// "" for an empty fleet.
func CommonEndpointVersion(fleet []EndpointDeploy) string {
	if len(fleet) == 0 {
		return ""
	}
	for _, d := range fleet[1:] {
		if d.Version != fleet[0].Version {
			return FleetVersionMixed
		}
	}
	if fleet[0].Version == "" {
		return FleetVersionMixed
	}
	return fleet[0].Version
}
