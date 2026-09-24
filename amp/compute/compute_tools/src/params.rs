pub const DEFAULT_LOG_LEVEL: &str = "info";

/// The rule that lets a client reach the compute from outside the pod; loopback
/// is covered by the `trust` rules the basebackup's `pg_hba.conf` already has.
///
/// This said `md5` for a long time, on the reasoning that Postgres upgrades an
/// `md5` rule to SCRAM for a role whose stored verifier is a SCRAM one — true,
/// but it made the keyword a no-op rather than a policy, and the control plane
/// was in fact handing Postgres finished md5 verifiers (yugabyte/meko#1351). Now
/// that every verifier is SCRAM, naming SCRAM here is what makes the downgrade
/// impossible: `md5` would still accept a role that somehow holds an md5
/// verifier, and there is no falling through to a later rule after a failed
/// auth, so the strict keyword is the only way to say it.
///
/// PGDATA is rebuilt from a basebackup on every compute start, so this takes
/// effect for an EXISTING endpoint the moment it is rolled onto an image
/// carrying it — an md5-era role then cannot authenticate until it is
/// re-passworded. See the migration note in `ac-k8s-deploy.md`.
pub const PG_HBA_ALL_SCRAM: &str = "host\tall\t\tall\t\tall\t\tscram-sha-256";
