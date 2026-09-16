package ingress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// ShardState is where a shard is in its life.
type ShardState string

const (
	// StateBuilding is a shard whose release is installed but whose load
	// balancer the cloud has not addressed yet. Deliberately NOT allocatable:
	// an endpoint placed here would publish a DNS record with an empty target.
	StateBuilding ShardState = "building"
	// StateReady is allocatable.
	StateReady ShardState = "ready"
	// StateDraining is on its way out: no new allocations, and reclaimable once
	// empty. Nothing writes it yet — the reclaimer is not built (ac-ingress.md,
	// "Not yet built") — so it exists here to keep the enum and this type in
	// step with 0018 rather than because anything transitions into it.
	StateDraining ShardState = "draining"
)

// Shard is one load balancer, its proxy tier, and the port range it serves.
type Shard struct {
	Name     string
	Family   Family
	State    ShardState
	Hostname string
	// RecordType is CNAME where the cloud gave the load balancer a hostname and
	// A where it gave an address, which is the only difference between the
	// providers that matters to an endpoint's DNS record.
	RecordType     string
	PortLo, PortHi int
}

// AddressLookup reports where a shard's load balancer answers.
//
// An interface rather than a dependency on the deployer: this package owns the
// shard state machine and knows nothing about Kubernetes, and the deployer knows
// how to read a Service and nothing about allocation. deploy.KubernetesDeployer
// satisfies it.
type AddressLookup interface {
	// ShardAddress reports where a shard answers. A non-nil error is a lookup
	// that failed — no such Service, no RBAC to read it, no cluster to ask;
	// `addressed` false with no error is a Service the cloud has not given an
	// address to yet. The two need telling apart: only the second is worth
	// waiting through, and both otherwise leave a shard in `building` forever.
	ShardAddress(ctx context.Context, shard string) (host string, isHostname, addressed bool, err error)

	// ShardPorts reports the listener ports the shard's Service actually
	// publishes, and the family its release was installed as. Read from the same
	// object as the address, and used to refuse promoting a shard whose release
	// disagrees with the row — see ReconcileShards.
	ShardPorts(ctx context.Context, shard string) (ports []int, family string, err error)
}

// ErrShardPortRangeChanged is returned when an existing shard's stored range
// differs from what the controller was configured with.
var ErrShardPortRangeChanged = errors.New("ingress shard port range differs from the configured one")

// EnsureShard registers a shard, idempotently.
//
// Called on every controller start for the first shard, so it has to converge
// rather than conflict: a restart is the normal case, not an error. It writes
// NOTHING onto an existing row. State and hostname belong to ReconcileShards,
// and overwriting them here would knock a ready shard back to building on every
// restart and stall every allocation until the next pass.
//
// The port range is the field worth being explicit about, because reconciling it
// from config looks harmless and is not. The range describes the listeners the
// shard's installed release actually serves; it is provisioned in full when the
// load balancer is built and never widened afterwards (0018). So a controller
// restarted with an edited --deploy-ingress-port-range, before anything upgrades
// that release, would widen the row and the allocator would start handing out
// ports nothing is listening on — endpoints that deploy and are simply
// unreachable. Narrowing is worse: live allocations outside the new range stay
// put, and Capacity subtracts a count of them from a width that no longer covers
// them, so free under-reports and can go negative.
//
// A mismatch is therefore refused rather than applied. Changing a live shard's
// range means upgrading its release first; the two have to move together, and
// nothing here can tell whether they did.
func (a *Allocator) EnsureShard(ctx context.Context, s Shard) error {
	var lo, hi int
	err := a.pool.QueryRow(ctx,
		`INSERT INTO amp_registry.ingress_shards
		     (name, family, state, port_lo, port_hi)
		 VALUES ($1, $2::amp_registry.ingress_family, 'building', $3, $4)
		 ON CONFLICT (name) DO UPDATE
		    SET name = amp_registry.ingress_shards.name
		 RETURNING port_lo, port_hi`,
		s.Name, string(s.Family), s.PortLo, s.PortHi).Scan(&lo, &hi)
	if err != nil {
		return fmt.Errorf("register ingress shard %s: %w", s.Name, err)
	}
	if lo != s.PortLo || hi != s.PortHi {
		return fmt.Errorf("ingress shard %s serves %d-%d but this controller is configured for %d-%d: %w "+
			"(the range is provisioned when the shard's load balancer is built, so changing it means "+
			"upgrading that release too — the row is not rewritten from config)",
			s.Name, lo, hi, s.PortLo, s.PortHi, ErrShardPortRangeChanged)
	}
	return nil
}

// MarkReady records where a shard answers and makes it allocatable.
func (a *Allocator) MarkReady(ctx context.Context, name, hostname, recordType string) error {
	_, err := a.pool.Exec(ctx,
		`UPDATE amp_registry.ingress_shards
		    SET state = 'ready', hostname = $2, record_type = $3::amp_registry.dns_record_type
		  WHERE name = $1 AND state = 'building'`,
		name, hostname, recordType)
	if err != nil {
		return fmt.Errorf("mark ingress shard %s ready: %w", name, err)
	}
	return nil
}

// ListShards returns every shard, whatever its state.
func (a *Allocator) ListShards(ctx context.Context) ([]Shard, error) {
	rows, err := a.pool.Query(ctx,
		`SELECT name, family, state, hostname, record_type, port_lo, port_hi
		   FROM amp_registry.ingress_shards ORDER BY created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("list ingress shards: %w", err)
	}
	defer rows.Close()

	var shards []Shard
	for rows.Next() {
		var s Shard
		if err := rows.Scan(&s.Name, &s.Family, &s.State,
			&s.Hostname, &s.RecordType, &s.PortLo, &s.PortHi); err != nil {
			return nil, fmt.Errorf("list ingress shards: %w", err)
		}
		shards = append(shards, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list ingress shards: %w", err)
	}
	return shards, nil
}

// ReconcileShards promotes every building shard whose load balancer the cloud
// has now addressed.
//
// Idempotent and cheap, so it is safe to run on a timer. Best-effort per shard:
// one cloud that has not finished provisioning must not stop another shard from
// being promoted, so a lookup that comes back empty is logged at debug and
// retried next pass rather than failing the sweep.
func (a *Allocator) ReconcileShards(ctx context.Context, look AddressLookup, logger *slog.Logger) error {
	shards, err := a.ListShards(ctx)
	if err != nil {
		return err
	}
	for _, s := range shards {
		if s.State != StateBuilding {
			continue
		}
		host, isHostname, addressed, err := look.ShardAddress(ctx, s.Name)
		switch {
		case err != nil:
			// The lookup itself failed: no such Service, no RBAC to read it, no
			// cluster to ask. Distinct from "no address yet" and worth a line at a
			// level somebody sees, because none of these clear on their own and the
			// shard is unallocatable until they do.
			logger.Warn("ingress shard address lookup failed", "shard", s.Name, "err", err)
			continue
		case !addressed:
			logger.Debug("ingress shard still building", "shard", s.Name)
			continue
		}
		// The row and the release are installed separately — the row from the
		// controller's config, the release from its chart's values — and nothing
		// makes them agree. Two ways they can diverge, both silent and both
		// serious, so the Service is checked against the row before it is ever
		// made allocatable.
		//
		// A narrower release than the row means the allocator hands out ports
		// nothing is listening on: routes that name an entryPoint which does not
		// exist, which Traefik accepts and never serves. A release installed as
		// the wrong FAMILY is worse — an internal shard actually fronted by an
		// internet-facing load balancer publishes every ingress_internal endpoint
		// on it, with no error anywhere.
		if err := a.verifyShardRelease(ctx, look, s); err != nil {
			logger.Error("ingress shard release disagrees with its row; not promoting",
				"shard", s.Name, "err", err)
			continue
		}
		recordType := "A"
		if isHostname {
			recordType = "CNAME"
		}
		if err := a.MarkReady(ctx, s.Name, host, recordType); err != nil {
			return err
		}
		logger.Info("ingress shard ready", "shard", s.Name, "family", s.Family,
			"host", host, "record_type", recordType, "ports", fmt.Sprintf("%d-%d", s.PortLo, s.PortHi))
	}
	return nil
}

// verifyShardRelease refuses a shard whose installed Service does not cover the
// row's port range, or was installed as a different family.
//
// A lookup that fails is NOT a mismatch: the shard simply stays building and the
// next pass tries again, the same as an address that has not arrived.
func (a *Allocator) verifyShardRelease(ctx context.Context, look AddressLookup, s Shard) error {
	ports, family, err := look.ShardPorts(ctx, s.Name)
	if err != nil {
		return fmt.Errorf("read shard %s ports: %w", s.Name, err)
	}
	if family != "" && family != string(s.Family) {
		return fmt.Errorf("shard %s is registered as family %q but its release was installed as %q: "+
			"promoting it would put %s endpoints behind the wrong load balancer",
			s.Name, s.Family, family, s.Family)
	}
	have := make(map[int]struct{}, len(ports))
	for _, p := range ports {
		have[p] = struct{}{}
	}
	var missing []int
	for p := s.PortLo; p <= s.PortHi; p++ {
		if _, ok := have[p]; !ok {
			missing = append(missing, p)
			if len(missing) == 5 {
				break
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("shard %s is registered for ports %d-%d but its release does not publish %v: "+
			"an endpoint allocated one of those would render a route naming an entryPoint that does not "+
			"exist, which Traefik accepts and never serves",
			s.Name, s.PortLo, s.PortHi, missing)
	}
	return nil
}

// FirstShardName is the first shard of a family, named rather than generated so
// EnsureShard converges on restart instead of registering a second row for the
// same release every time.
//
// The release it names is installed alongside the control plane by a chart that
// lands later in this series; until then the row this registers stays `building`
// because there is no Service to read an address from, and ReconcileShards says
// so on every pass.
//
// Nothing marks it special in the schema, and nothing protects it yet: the
// intended floor is "never drain a family's last ready shard", which would cover
// this one without recording that it was first — but 0018 states that rule is
// deliberately not in the schema, and the reclaimer that would enforce it does
// not exist.
const FirstShardName = "pgin-0"

// FirstShardNameFor names the first shard of a family. The families are separate
// load balancers with separate exposures, so each needs its own first shard —
// registering one name for both would give an external-only deployment a shard
// in the family Allocate refuses and none in the family it serves.
//
// Internal keeps the bare FirstShardName so the common single-family deployment
// reads as it did, and so an existing row is not orphaned by a rename.
func FirstShardNameFor(f Family) string {
	if f == FamilyExternal {
		return FirstShardName + "-ext"
	}
	return FirstShardName
}
