-- 0007: shared Postgres ingress — shards, and the port each endpoint holds on one.
--
-- An endpoint reachable through the shared ingress is addressed as
-- `<dns_name>:<port>` on a SHARD: one cloud load balancer plus a proxy tier,
-- serving a fixed, pre-provisioned port range. Creating an endpoint then
-- allocates a free port on a ready shard rather than provisioning anything in
-- the cloud, which is the whole point of the design — see
-- controller/architecture/ac-ingress.md.
--
-- The indexes share one transaction with the tables they cover: golang-migrate
-- runs each file in one, and YugabyteDB downgrades a CREATE INDEX inside a
-- transaction block to a non-concurrent build (yugabyte-db#6240) whose backfill
-- does not see rows written earlier in that same transaction. Both tables are
-- created empty here, so there is nothing for a backfill to miss. An index added
-- later to a table that already holds rows does not have that luxury.

DO $$ BEGIN
    CREATE TYPE amp_registry.ingress_family AS ENUM ('internal', 'external');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- `building` is the state a shard occupies between its release being installed
-- and the cloud handing its load balancer an address. It is deliberately NOT
-- allocatable: an endpoint placed there would render a DNS record whose target
-- is still empty.
DO $$ BEGIN
    CREATE TYPE amp_registry.ingress_shard_state AS ENUM ('building', 'ready', 'draining');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- AWS hands a load balancer a hostname and the other providers hand out an
-- address, so the record an endpoint publishes is a CNAME on one and an A record
-- on the others. Carried per shard because it is a property of how that shard's
-- load balancer was provisioned, not of the deployment.
DO $$ BEGIN
    CREATE TYPE amp_registry.dns_record_type AS ENUM ('CNAME', 'A');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS amp_registry.ingress_shards (
    -- The release name, so the row and the Helm release share one identifier.
    name        text PRIMARY KEY,

    family      amp_registry.ingress_family      NOT NULL,
    state       amp_registry.ingress_shard_state NOT NULL,

    -- Where this shard's load balancer answers. Empty until the cloud assigns
    -- it, which is what `building` means; an endpoint's DNS record is rendered
    -- from this, so it is read once per shard rather than once per endpoint.
    hostname    text                             NOT NULL DEFAULT '',
    record_type amp_registry.dns_record_type     NOT NULL DEFAULT 'CNAME',

    -- The listener range, provisioned in full when the shard is built. Endpoint
    -- creation allocates from it and never widens it, so no endpoint lifecycle
    -- event mutates a load balancer that live connections sit behind.
    port_lo     integer                          NOT NULL,
    port_hi     integer                          NOT NULL,

    created_at  timestamptz                      NOT NULL DEFAULT now(),
    drained_at  timestamptz,

    CONSTRAINT ingress_shards_port_range CHECK (port_hi >= port_lo),
    CONSTRAINT ingress_shards_ports_valid CHECK (port_lo > 0 AND port_hi < 65536)
);

-- Reclaim's floor — a family's last ready shard is never drained, whoever
-- installed it — is deliberately NOT expressed here. It is a rule about a
-- transition, which a CHECK cannot see, and the reclaimer that will enforce it
-- does not exist yet. Said out loud because the alternative is a reader assuming
-- the column set holds it: `state` and `drained_at` are unguarded, and nothing
-- in the tree reads or writes this table today.
--
-- The allocator's hot path: the emptiest READY shard of the requested family.
CREATE INDEX IF NOT EXISTS ingress_shards_allocatable
    ON amp_registry.ingress_shards (family)
    WHERE state = 'ready';

CREATE TABLE IF NOT EXISTS amp_registry.ingress_allocations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),

    -- NOT a foreign key, and that is load-bearing. An endpoint is hard-deleted
    -- (core.pg.go, `DELETE FROM amp_registry.endpoints`), so a cascading
    -- reference would take this row with it and free the port the instant the
    -- endpoint went away. The quarantine below exists precisely to stop that: a
    -- client holding a cached address must not land on whichever database next
    -- takes the slot. The row therefore outlives the endpoint by design, and the
    -- reclaim sweep — not the database — is what eventually removes it.
    endpoint_id uuid        NOT NULL,

    -- Kept referential, unlike endpoint_id: a shard must not be dropped while
    -- any allocation still points at it, quarantined ones included, because its
    -- DNS records can still be resolving.
    shard       text        NOT NULL REFERENCES amp_registry.ingress_shards (name),
    port        integer     NOT NULL,

    -- The customer-facing name, derived from the endpoint's immutable id rather
    -- than from anything renameable. Stored rather than recomputed so the
    -- address an endpoint was issued is a fact on the row, not a function of
    -- config that may since have changed.
    dns_name    text        NOT NULL,

    created_at  timestamptz NOT NULL DEFAULT now(),

    -- Set on retire instead of deleting the row, so the release has a time and
    -- the reclaim sweep has something to age.
    --
    -- The QUARANTINE is not enforced here. Nothing below filters on this
    -- column's age: the moment it is set the slot leaves every unique index and
    -- the database will happily reissue it. Holding it back is the allocator's
    -- job, in its candidate query — so a second allocation path has to carry
    -- that filter too, and the database will not catch one that forgets.
    released_at timestamptz
);

-- One live allocation per endpoint, and a trail of released ones behind it.
--
-- The trail is what the synthetic primary key buys: a key on endpoint_id would
-- allow exactly one row ever, so a re-allocated endpoint would overwrite the
-- record of where it used to answer.
--
-- Note what this FORBIDS, because it constrains the allocator built on top. Two
-- live rows for one endpoint cannot coexist, so moving an endpoint between
-- shards is release-then-allocate, never the reverse. There is therefore a
-- window in which the endpoint holds no allocation, and a crash inside it leaves
-- it that way until its next deploy re-allocates. Survivable because a move
-- changes the endpoint's port anyway — the client reconnects regardless, so
-- there was never a seamless hand-over to protect. Wanting one would mean
-- admitting the overlap here (keyed per shard, or gated on a state column), and
-- that is another migration rather than a code change.
CREATE UNIQUE INDEX IF NOT EXISTS ingress_allocations_live_per_endpoint
    ON amp_registry.ingress_allocations (endpoint_id)
    WHERE released_at IS NULL;

-- The invariant the whole design rests on: no two live endpoints share a slot,
-- and no two share a name. Enforced in the schema rather than only in the
-- allocator, because the failure it forecloses is a connection reaching another
-- customer's database.
CREATE UNIQUE INDEX IF NOT EXISTS ingress_allocations_slot_live
    ON amp_registry.ingress_allocations (shard, port)
    WHERE released_at IS NULL;

CREATE UNIQUE INDEX IF NOT EXISTS ingress_allocations_dns_live
    ON amp_registry.ingress_allocations (dns_name)
    WHERE released_at IS NULL;

-- Deliberately covers RELEASED rows too, unlike the unique index above: the
-- allocator's probe asks whether a slot is free, and a quarantined slot is not.
-- An index restricted to live rows would answer that question wrongly.
CREATE INDEX IF NOT EXISTS ingress_allocations_slot
    ON amp_registry.ingress_allocations (shard, port);

-- The reclaim sweep's index: released rows whose quarantine has expired, which
-- is a range scan on released_at.
--
-- `ASC` is load-bearing rather than decoration, for the reason 0002 measured on
-- branches_gc_due and tasks_by_updated_at (0001) hit again: YSQL defaults a secondary index's leading
-- column to HASH, and a hash-partitioned column cannot serve a range predicate,
-- so the scan degrades to a storage filter plus sort (10k rows in 627 ms there,
-- against 5 due rows in 2 ms with ASC). Added now rather than alongside the
-- sweep, because adding it afterwards is another migration.
CREATE INDEX IF NOT EXISTS ingress_allocations_due
    ON amp_registry.ingress_allocations (released_at ASC)
    WHERE released_at IS NOT NULL;
