// Package pglease is a Go client for the resource lease defined in migration
// 0004_resource_token: it calls amp_registry.token_acquire / token_release /
// token_holder rather than restating the rule, so this client and the Rust write
// fence share one definition. It requires that migration to have run.
//
// A lease is an absolute expiry: epoch microseconds, read from the DATABASE
// clock rather than from any client's, at which the holder must stop writing.
// One value is therefore both "you hold the lease" and "stop writing at this
// instant", which is what lets a service that was handed the value decide for
// itself, against its own clock, without talking to anything.
//
// Mutual exclusion comes entirely from one conditional upsert: a row is written
// only when the incoming owner already holds the lease, or the stored expiry
// plus the clock-skew grace has passed. "Granted" is a fact about that write,
// never a conclusion drawn from a prior read.
//
// Renewal and takeover are deliberately asymmetric. The holder renews with no
// time condition at all — a renewal cannot violate mutual exclusion whoever's
// clock is right — so a holder whose lease lapsed arbitrarily long ago still
// gets it back provided nobody took over. A different owner may take over only
// once expiry plus skew has passed. Release is owner-scoped and skips the grace
// entirely: an explicit handoff needs no clock agreement.
//
// Client keeps a lease renewed in the background and fails the process fast the
// moment ownership can no longer be proven: a renewal was refused, or the
// database stamped one after the expiry already held (so ownership lapsed in
// between, whatever the answer says), or none was confirmed in time. A fence has
// to remove the writer, not the write: see AbortProcess.
//
// One Client holds a key once. A second Acquire of a held key is refused with
// ErrAlreadyHeld rather than granted as the renewal the database would take it
// for.
package pglease
