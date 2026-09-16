// Package ingress allocates a port on a shared-ingress shard to an endpoint,
// and gives it back when the endpoint retires.
//
// A shard is one cloud load balancer plus a proxy tier serving a fixed,
// pre-provisioned port range (controller/architecture/ac-ingress.md). Creating
// an endpoint therefore takes a free slot on an existing shard rather than
// provisioning anything in the cloud, which is what keeps endpoint creation off
// the cloud's critical path.
//
// This lives outside core.Backend deliberately. Backend is the interface for
// user-facing resources — users, pools, clusters, branches, endpoints — whereas
// a shard is capacity the control plane manages on its own behalf, and the
// allocator is the one place where a bug hands two customers the same address.
// A narrow package is easier to reason about and to test than five more methods
// on that interface.
package ingress

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"math/rand/v2"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/yugabyte/amp/controller/internal/db"
)

// ErrNoCapacity is returned when the requested family has no ready shard with a
// free slot — whether because none is registered yet, because they are all still
// building, or because every slot is taken or cooling off in quarantine. The
// three are not separated because the answer to all of them is the same: come
// back when there is capacity. What decides whether to BUILD capacity reads
// Capacity, not this error.
//
// It is deliberately NOT a fallback to some other connectivity mode: quietly
// provisioning a dedicated load balancer instead is how a deployment ends up
// back at the per-endpoint bill this design exists to remove, and it would do so
// invisibly.
var ErrNoCapacity = errors.New("no ingress capacity")

// ErrNotConfigured is returned when this deployment does not route endpoints
// through shards at all (--deploy-ingress-enabled is off). Separate from
// ErrNoCapacity because the two want opposite handling: capacity arrives on its
// own once a shard is built, whereas this is a misconfiguration and the intended
// surface is a 400 rather than a retry.
var ErrNotConfigured = errors.New("shared ingress is not configured")

// ErrNotAllocated is returned when an endpoint has no live allocation.
var ErrNotAllocated = errors.New("endpoint has no ingress allocation")

// Placement decides which ready shard a new endpoint lands on, and it is a real
// trade rather than a tuning detail.
//
// Spread puts it on the emptiest shard: the fleet stays balanced and one proxy
// outage takes down the fewest databases. It also means no shard ever empties on
// its own, so reclaiming one is only possible by MOVING endpoints — and on the
// port path a move changes the port, which changes the customer's connection
// string. Spread therefore trades away the ability to ever give a shard back.
//
// Pack puts it on the fullest shard that still has room, so retirements
// concentrate and shards drain to empty by themselves — reclaimable without
// moving anybody. The cost is a larger blast radius per outage while the fleet
// is partly full.
//
// Once endpoints are addressed by name alone (the shared-port path), a move
// becomes a DNS retarget with no client-visible change, and defragmentation
// makes spread reclaimable too. Until then this is the lever.
type Placement string

const (
	PlacementSpread Placement = "spread"
	PlacementPack   Placement = "pack"
)

// orderBy renders the placement policy as the ORDER BY the allocator runs.
// Ascending `used` takes the emptiest shard; descending takes the fullest one
// that still had a free slot to offer.
func (p Placement) orderBy() string {
	if p == PlacementPack {
		return "r.used DESC, r.name, p.port"
	}
	return "r.used, r.name, p.port"
}

// Family is the exposure a shard provides. It is a property of the shard's load
// balancer, so an endpoint can never land on the wrong one by accident.
type Family string

const (
	FamilyInternal Family = "internal"
	FamilyExternal Family = "external"
)

// Zones is the DNS zone each family's endpoint names are published under.
//
// Per family rather than one zone for both, because external-dns is pinned to a
// zone with --zone-id-filter and the whole point of the internal family is that
// its names are not resolvable publicly (ac-ingress.md, "Dependencies outside
// the controller"). A single shared zone would publish an internal endpoint's
// name into whichever zone happened to be configured — and dns_name is durable
// (0018 stores it on the row), so correcting that later means reissuing
// addresses customers already hold, not a config change.
//
// Either may be empty. A family with no zone still gets a name allocated — an
// unqualified one, `ep-xxxxxxxx` — which is simply never published; clients are
// given the shard's own address instead. The name's SHAPE is what records that
// decision, durably, so a redeploy cannot reach a different conclusion.
type Zones struct {
	Internal string
	External string
}

// For returns the zone a family's names are published under, or empty.
func (z Zones) For(f Family) string {
	if f == FamilyExternal {
		return z.External
	}
	return z.Internal
}

// Allocation is one endpoint's place on a shard: the port it answers on and the
// name published for it.
type Allocation struct {
	ID         uuid.UUID
	EndpointID uuid.UUID
	Shard      string
	Port       int
	DNSName    string
	// ShardHost and RecordType are the shard's load-balancer address and how to
	// publish it — carried on the allocation so the caller renders the endpoint's
	// DNS record from one read rather than looking the shard up again.
	ShardHost  string
	RecordType string
	// Publish reports whether DNSName should be published and handed to clients.
	// False when this endpoint's family has no DNS zone — a kind or CI cluster
	// with no external-dns, say — where the honest address is the shard's own.
	Publish   bool
	CreatedAt time.Time
}

// Address is what a client connects to: the endpoint's published name where
// there is DNS to publish it, and the shard's own address where there is not.
//
// The fallback is not a degraded mode so much as a plainer one. It gives up the
// property that the shard is invisible — moving an endpoint between shards would
// change this address — which is worth nothing on a cluster where nothing moves,
// and it is the difference between routing identically on a kind or CI cluster
// and not running there at all.
func (a Allocation) Address() string {
	host := a.ShardHost
	if a.Publish && a.DNSName != "" {
		host = a.DNSName
	}
	return fmt.Sprintf("%s:%d", host, a.Port)
}

// Allocator hands out and reclaims slots.
type Allocator struct {
	pool *db.Pool
	// enabled is the feature switch. Separate from the zones because the two are
	// different questions: whether this deployment routes through shards at all,
	// and whether it has DNS to publish the names in. A CI cluster answers yes
	// and no.
	enabled bool
	// zones are the DNS zones endpoint names are published under, one per family.
	// A family with no zone still gets a name allocated — just not published, and
	// its clients are handed the shard's own address.
	zones Zones
	// quarantine is how long a released slot stays out of the pool. It exists so
	// a client holding a cached address cannot land on whichever database next
	// takes the slot, so it should comfortably exceed the record's TTL.
	quarantine time.Duration
	// placement decides spread vs pack; see Placement.
	placement Placement
}

// New builds an allocator. A deployment with ingress disabled refuses to
// allocate; one with no zone for a family allocates the name but does not
// publish it. Placement defaults to spread when unset.
func New(pool *db.Pool, enabled bool, zones Zones, quarantine time.Duration, placement Placement) *Allocator {
	if placement == "" {
		placement = PlacementSpread
	}
	return &Allocator{pool: pool, enabled: enabled, zones: zones, quarantine: quarantine, placement: placement}
}

// Enabled reports whether this deployment routes endpoints through shards at
// all.
//
// Meant for the API layer to refuse the ingress connectivity modes up front, so
// a caller gets a 400 rather than an endpoint that deploys and then cannot be
// reached. Nothing calls it yet — the API refuses on the config flag instead
// (handlers.go) — so this is the intended contract, not a live caller.
func (a *Allocator) Enabled() bool { return a.enabled }

// PublishesDNS reports whether this family has a zone to publish its names in.
// False leaves the names allocated but unpublished.
func (a *Allocator) PublishesDNS(family Family) bool { return a.zones.For(family) != "" }

// publishable reports whether a STORED name can be handed to a client. A name
// minted without a zone is unqualified, and stays that way: recomputing this
// from the current zones would make every pre-existing row look publishable the
// day somebody sets one, handing external-dns records outside the zone it is
// pinned to. Giving an existing fleet published names means reissuing them.
func publishable(dnsName string) bool { return strings.Contains(dnsName, ".") }

// dnsNameFor derives the published name from the endpoint's id.
//
// From the ID, never from the endpoint's name or its cluster's or pool's: those
// are renameable (0016_storage_pool_name_live), and an address that breaks when
// somebody renames a project is a broken address. `attempt` widens the prefix on
// the astronomically unlikely collision, so the pretty short form is what almost
// every endpoint gets while uniqueness stays guaranteed rather than probable.
func (a *Allocator) dnsNameFor(endpointID uuid.UUID, family Family, attempt int) string {
	hex := strings.ReplaceAll(endpointID.String(), "-", "")
	n := 8
	if attempt > 0 {
		n = len(hex)
	}
	// Still allocated, and still unique, when there is no zone to publish it in:
	// the column and its uniqueness index do not change shape just because this
	// deployment has no DNS, and the name is then ready the day it gets one.
	zone := a.zones.For(family)
	if zone == "" {
		return fmt.Sprintf("ep-%s", hex[:n])
	}
	return fmt.Sprintf("ep-%s.%s", hex[:n], zone)
}

// allocateSQLTemplate takes the lowest free port on whichever ready shard of the
// family the placement policy picks — the emptiest under spread, the fullest
// with room under pack — and inserts the allocation in the same statement.
//
// One statement rather than select-then-insert on purpose: between a read and a
// write, a concurrent create can take the slot we just chose. Here the unique
// indexes from 0018 are the arbiter — the loser gets a 23505 and retries — so
// correctness does not depend on the allocator being the only writer.
//
// `used` is computed once per shard rather than once per candidate port, which
// matters because the port range is expanded with generate_series.
//
// The ORDER BY is interpolated rather than bound, which is safe here and only
// here: Placement.orderBy returns one of two compile-time constants and no
// caller-supplied string ever reaches it. Every value in the statement is a bind
// parameter.
//
// The quarantine window is bound as SECONDS through make_interval rather than as
// a Go duration string cast to interval. Postgres does happen to parse "10m0s"
// and friends, but that is two grammars coinciding rather than a contract, and
// it is not one to discover has stopped holding on a different engine.
const allocateSQLTemplate = `
WITH ready AS (
    SELECT s.name, s.port_lo, s.port_hi, s.hostname, s.record_type,
           (SELECT count(*) FROM amp_registry.ingress_allocations l
             WHERE l.shard = s.name AND l.released_at IS NULL) AS used
      FROM amp_registry.ingress_shards s
     WHERE s.family = $3::amp_registry.ingress_family
       AND s.state = 'ready'
), slot AS (
    SELECT r.name, p.port, r.hostname, r.record_type
      FROM ready r
      CROSS JOIN LATERAL generate_series(r.port_lo, r.port_hi) AS p(port)
     WHERE NOT EXISTS (
         SELECT 1 FROM amp_registry.ingress_allocations e
          WHERE e.shard = r.name AND e.port = p.port
            AND (e.released_at IS NULL OR e.released_at > now() - make_interval(secs => $4)))
     ORDER BY %s
     LIMIT 1
)
INSERT INTO amp_registry.ingress_allocations (endpoint_id, shard, port, dns_name)
SELECT $1::uuid, slot.name, slot.port, $2 FROM slot
RETURNING id, endpoint_id, shard, port, dns_name, created_at,
          (SELECT hostname FROM slot), (SELECT record_type::text FROM slot)`

// Allocate gives the endpoint a slot on a shard of the requested family.
//
// IDEMPOTENT: an endpoint that already holds a live slot gets that same slot
// back, never a second one. This is the property that makes the call usable
// inside a task step, which is the unit of re-run both on a 40001 abort and on
// resume after a crash (task/worker.go) — an Allocate that answered a replay
// with an error would make any step containing it fail permanently the second
// time it ran.
//
// It also closes the unknown-outcome window, which is the part with no caller-
// side fix: a connection that dies under the INSERT may still have committed it,
// and db.Unavailable is explicitly not db.SafeToRetry. The re-run then finds the
// row it created rather than colliding with it forever — and because
// ingress_allocations has no foreign key to endpoints and the reclaim sweep only
// looks at released rows, a live orphan would otherwise leak its slot for good.
//
// Returning the existing row is right rather than merely convenient: the
// allocation IS the record of where an endpoint answers, so re-deriving an
// address a client may already hold would change it. Moving an endpoint to
// another family is therefore not something this does — Release first, then
// Allocate, which is the order 0018's live-per-endpoint index forces anyway.
//
// The existing-slot check runs BEFORE the insert rather than classifying the
// error after it, because after it we cannot tell which constraint fired. On
// YSQL a unique violation carries only its SQLSTATE and a message —
// ConstraintName and Detail are empty, unlike upstream PostgreSQL — so a
// classifier reading that field never fires, which is what made every concurrent
// resize a 500 instead of a 409 (#400, see task.taskLockViolation). The check is
// advisory rather than a lock: two concurrent Allocates for one endpoint can
// both pass it, and the live-per-endpoint index is what actually stops the
// second — whose retry then re-reads, finds the winner's row, and returns it.
func (a *Allocator) Allocate(ctx context.Context, endpointID uuid.UUID, family Family) (Allocation, error) {
	if !a.Enabled() {
		return Allocation{}, fmt.Errorf("allocate ingress slot for %s: %w", endpointID, ErrNotConfigured)
	}
	switch existing, err := a.Get(ctx, endpointID); {
	case err == nil:
		return existing, nil
	case !errors.Is(err, ErrNotAllocated):
		return Allocation{}, err
	}
	name := a.dnsNameFor(endpointID, family, 0)
	var lastErr error
	for attempt := range allocateAttempts {
		if attempt > 0 {
			if err := pause(ctx, attempt); err != nil {
				return Allocation{}, err
			}
		}
		alloc, err := a.allocateOnce(ctx, endpointID, family, name)
		switch {
		case err == nil:
			return alloc, nil
		case !isUniqueViolation(err):
			return Allocation{}, err
		}
		lastErr = err
		// Somebody committed between our candidate query and our insert. Re-read
		// before deciding what lost: if the winner was another Allocate for THIS
		// endpoint, its row is the one we owe the caller.
		switch existing, gerr := a.Get(ctx, endpointID); {
		case gerr == nil:
			return existing, nil
		case !errors.Is(gerr, ErrNotAllocated):
			return Allocation{}, gerr
		}
		// So it was the slot or the name, and on YSQL the error will not say
		// which. They need opposite responses, which is why this asks rather than
		// widening on any violation: a slot race needs no help — the winner's row
		// is committed, so the next candidate query skips that port — while a name
		// collision repeats forever unless the name changes. The two also differ
		// enormously in how often they happen. The ORDER BY makes every concurrent
		// Allocate in a family pick the SAME lowest free port, so overlapping
		// creates collide on the slot BY CONSTRUCTION, whereas colliding on the
		// name takes two endpoint ids sharing their first 32 bits. Widening on any
		// violation would hand an ordinary race's loser the 32-hex name for the
		// life of the endpoint, with nothing having collided on its name.
		taken, err := a.dnsNameTaken(ctx, name)
		if err != nil {
			return Allocation{}, err
		}
		if taken {
			name = a.dnsNameFor(endpointID, family, 1)
		}
	}
	return Allocation{}, fmt.Errorf("allocate ingress slot for %s: %w", endpointID, lastErr)
}

// allocateAttempts bounds the retry, and it has to be enough on its own: a 23505
// is not a 40001, so runStepWithRetry (task/worker.go) does NOT re-run a step
// that lets one escape — it is a permanent task failure. The loop only spins on
// a LOST RACE, since an exhausted family returns ErrNoCapacity from the
// candidate query and stops immediately, so this is sized well above the
// concurrent creates one family should ever see rather than tuned.
const allocateAttempts = 8

// allocateBackoff spaces those retries. Two allocators that collide and then
// retry in lockstep collide again, which is what makes a back-to-back retry
// weaker than its attempt count suggests.
const allocateBackoff = 20 * time.Millisecond

// pause waits out a lost race, jittered so racers do not resynchronise.
func pause(ctx context.Context, attempt int) error {
	d := time.Duration(attempt)*allocateBackoff + time.Duration(rand.Int64N(int64(allocateBackoff)))
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// dnsNameTaken reports whether a live allocation already holds the name. Asked
// only after a unique violation, to learn which index fired — see Allocate.
func (a *Allocator) dnsNameTaken(ctx context.Context, name string) (bool, error) {
	var taken bool
	err := a.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM amp_registry.ingress_allocations
		                 WHERE dns_name = $1 AND released_at IS NULL)`,
		name).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("check ingress dns name %s: %w", name, err)
	}
	return taken, nil
}

func (a *Allocator) allocateOnce(ctx context.Context, endpointID uuid.UUID, family Family, dnsName string) (Allocation, error) {
	var alloc Allocation
	err := a.pool.QueryRow(ctx, fmt.Sprintf(allocateSQLTemplate, a.placement.orderBy()),
		endpointID.String(), dnsName, string(family), a.quarantine.Seconds(),
	).Scan(&alloc.ID, &alloc.EndpointID, &alloc.Shard, &alloc.Port, &alloc.DNSName, &alloc.CreatedAt,
		&alloc.ShardHost, &alloc.RecordType)
	alloc.Publish = publishable(alloc.DNSName)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The INSERT ... SELECT matched no slot. Still ErrNoCapacity either way —
		// the caller's move is the same, and "no shard yet" and "a shard is still
		// building" are genuinely indistinguishable from here — but the MESSAGE
		// says which, because they are not the same thing to an operator. A family
		// with no shard registered at all on a deployment that serves the other
		// one is a configuration answer, not a wait.
		var shards int
		if cerr := a.pool.QueryRow(ctx,
			`SELECT count(*) FROM amp_registry.ingress_shards
			  WHERE family = $1::amp_registry.ingress_family`,
			string(family)).Scan(&shards); cerr != nil || shards == 0 {
			return Allocation{}, fmt.Errorf(
				"allocate ingress slot for %s: %w — no shard is registered for family %s on this "+
					"deployment, so none will become free; a shard for that family has to be installed",
				endpointID, ErrNoCapacity, family)
		}
		return Allocation{}, fmt.Errorf("allocate ingress slot for %s: %w in family %s "+
			"(every ready shard is full, quarantined, or still building)",
			endpointID, ErrNoCapacity, family)
	case err != nil:
		return Allocation{}, err
	}
	return alloc, nil
}

// Release marks the endpoint's live allocation as returned, starting its
// quarantine. The row is kept rather than deleted — that is what the quarantine
// is measured from, and it is why ingress_allocations has no foreign key to
// endpoints, which are hard-deleted.
//
// It does NOT clear the copy of the address that lives on the endpoint row
// (endpoints.address, written by core's SetEndpointAddress). As of the lifecycle
// wiring, create_endpoint DOES write an ingress address there, so there is a
// copy that could go stale — and this stays safe only because every caller
// deletes the endpoint row in the next breath: delete_endpoint, and the
// delete_cluster and delete_branch cascades. A caller that releases WITHOUT
// deleting the endpoint, which a connectivity-mode change or a shard move would,
// has to clear it here too, or the endpoint keeps advertising a host:port that
// this quarantine is counting down to hand to somebody else.
//
// Idempotent: an endpoint with nothing live is already released.
func (a *Allocator) Release(ctx context.Context, endpointID uuid.UUID) error {
	_, err := a.pool.Exec(ctx,
		`UPDATE amp_registry.ingress_allocations
		    SET released_at = now()
		  WHERE endpoint_id = $1::uuid AND released_at IS NULL`,
		endpointID.String())
	if err != nil {
		return fmt.Errorf("release ingress slot for %s: %w", endpointID, err)
	}
	return nil
}

// Get returns the endpoint's live allocation, or ErrNotAllocated.
func (a *Allocator) Get(ctx context.Context, endpointID uuid.UUID) (Allocation, error) {
	var (
		alloc  Allocation
		family string
	)
	err := a.pool.QueryRow(ctx,
		`SELECT e.id, e.endpoint_id, e.shard, e.port, e.dns_name, e.created_at,
		        s.hostname, s.record_type::text, s.family::text
		   FROM amp_registry.ingress_allocations e
		   JOIN amp_registry.ingress_shards s ON s.name = e.shard
		  WHERE e.endpoint_id = $1::uuid AND e.released_at IS NULL`,
		endpointID.String(),
	).Scan(&alloc.ID, &alloc.EndpointID, &alloc.Shard, &alloc.Port, &alloc.DNSName, &alloc.CreatedAt,
		&alloc.ShardHost, &alloc.RecordType, &family)
	alloc.Publish = publishable(alloc.DNSName)
	if errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, fmt.Errorf("get ingress slot for %s: %w", endpointID, ErrNotAllocated)
	}
	if err != nil {
		return Allocation{}, fmt.Errorf("get ingress slot for %s: %w", endpointID, err)
	}
	return alloc, nil
}

// Capacity reports free and total slots across every ready shard of the family.
// Free counts quarantined slots as taken, because they are.
//
// Meant for the headroom watermark that decides when to build another shard:
// building one a little early is cheaper than discovering at allocation time
// that the free slots were all cooling off. That reconciler is not built yet
// (ac-ingress.md, "Not yet built"), so this has no callers — it is the reason
// the counting rule is what it is, stated where the next person will need it.
func (a *Allocator) Capacity(ctx context.Context, family Family) (free, total int, err error) {
	err = a.pool.QueryRow(ctx,
		`SELECT
		     COALESCE(sum(s.port_hi - s.port_lo + 1), 0) AS total,
		     COALESCE(sum(s.port_hi - s.port_lo + 1), 0) - (
		         SELECT count(*) FROM amp_registry.ingress_allocations e
		          JOIN amp_registry.ingress_shards t ON t.name = e.shard
		         WHERE t.family = $1::amp_registry.ingress_family AND t.state = 'ready'
		           AND (e.released_at IS NULL OR e.released_at > now() - make_interval(secs => $2))
		     ) AS free
		   FROM amp_registry.ingress_shards s
		  WHERE s.family = $1::amp_registry.ingress_family AND s.state = 'ready'`,
		string(family), a.quarantine.Seconds(),
	).Scan(&total, &free)
	if err != nil {
		return 0, 0, fmt.Errorf("ingress capacity for family %s: %w", family, err)
	}
	return free, total, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
