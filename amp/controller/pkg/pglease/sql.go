package pglease

// The lease rule lives in SQL, in migration 0006_resource_token: `token_acquire`
// decides a grant in one atomic upsert, `token_release` frees a lease the caller
// owns, and `token_holder` reports whoever is keeping others out. This package
// calls those functions rather than restating them, so there is one definition of
// the rule for both this client and the Rust one in
// compute/libs/utils/src/write_fence.rs to share. A second copy of the predicate
// here would be a second thing to keep in step, and the two could disagree while
// both looked right.
//
// The consequence is a hard dependency: migration 0006 must have run against the
// database this package is pointed at. There is no DDL here and no table of our
// own — leases live in `amp_registry.resource_token`, keyed by the caller's
// resource id, alongside every other holder.
//
// Everything is schema-qualified and search_path is never set on the connection:
// qualification is what makes a call correct whatever session the caller hands
// us, and a library must not depend on a session GUC.

// acquireStmt binds $1 resource_id, $2 client_id, $3 ttl_ms, $4 skew_ms.
//
// Always returns exactly one row: granted true carries the new expiry, granted
// false carries the current holder and how long until this client may take over.
// The function bounds ttl_ms to (0, 300000] and skew_ms to [0, 3000] and RAISEs
// outside them, so a bad argument is an error rather than a silently clamped
// lease; Config.Validate rejects the same ranges first, to fail without a round
// trip.
const acquireStmt = `SELECT granted, token, holder, retry_after_ms
                       FROM amp_registry.token_acquire($1::text, $2::text, $3::bigint, $4::bigint)`

// releaseStmt binds $1 resource_id, $2 client_id, and reports whether a row went
// away. Owner-scoped, so it is replay-safe and cannot free a lease this client
// no longer holds — including the one its own replacement took.
const releaseStmt = `SELECT amp_registry.token_release($1::text, $2::text)`

// holderStmt binds $1 resource_id, $2 skew_ms, and returns 0 or 1 rows.
//
// No row means free TO A NEW CLIENT: the function keeps reporting a lapsed lease
// through the skew grace, which is exactly the window in which an acquire would
// still be denied. That is why this answer and a subsequent acquire cannot
// disagree.
const holderStmt = `SELECT client_id, token, expires_in_ms, takeover_in_ms
                      FROM amp_registry.token_holder($1::text, $2::bigint)`
