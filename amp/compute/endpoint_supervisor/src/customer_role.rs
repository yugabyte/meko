//! A customer login that is not a superuser (AMP-15).
//!
//! Without `CUSTOMER_ROLE`, clients log in as `cloud_admin`, the bootstrap
//! superuser `initdb` created — and a superuser reaches the machine:
//! `COPY ... TO PROGRAM` is a shell as the postgres uid, and `pg_read_file`
//! reads the compute spec that names the shared backing database. With it,
//! clients log in as that role instead, and `cloud_admin` is left to the control
//! plane over loopback.
//!
//! The role is Neon's model, which `compute_ctl` carries but AMP never runs: a
//! `NOLOGIN` group role (`--privileged-role-name`, `yb_amp_superuser` by default)
//! holding create-db/role, replication, `BYPASSRLS` and the predefined
//! `pg_read_all_data` / `pg_write_all_data`, layered with `compute_ctl`'s
//! migrations, and the customer a member of it. `compute_ctl` would create all
//! of that in `apply_spec_sql`, but `skip_pg_catalog_updates` — which every amp
//! spec sets — skips it, and turning the flag off would re-enable every other
//! phase of catalog reconciliation too. So the supervisor runs the same SQL
//! itself, over the loopback connection [`crate::remote_auth`] already holds.
//!
//! The SQL is `compute_tools`' own files, not a copy: the group role from
//! `sql/create_privileged_role.sql` with the privileges `spec_apply.rs` gives it,
//! and the migrations tracked in `yb_amp_migration.migration_id` exactly as
//! `compute_ctl`'s `MigrationRunner` tracks them — so a compute that later runs
//! the runner itself starts where this left off rather than re-running 0002.
//!
//! What compute_ctl would NOT do, and this does: grant the group role `CREATE`
//! on the `postgres` database and its `public` schema. `compute_ctl` fixes schema
//! ownership only in user databases (`ChangeSchemaPerms` skips the system one),
//! but `postgres` is the database amp's clients use, and since PG15 its `public`
//! schema takes `CREATE` from its owner alone — which is `cloud_admin`.

/// `compute-ctl`'s default `--privileged-role-name`: the group role's name when
/// `PRIVILEGED_ROLE_NAME` is unset, since `boot::compute_ctl_argv` passes the
/// flag only when it is set. The group role's name has to be the one
/// `compute_ctl` writes into
/// `yb_amp.privileged_role_name`, since that is the name the `yb_amp` DDL handler
/// grants its event-trigger and role-management exemptions to.
pub const DEFAULT_PRIVILEGED_ROLE: &str = "yb_amp_superuser";

/// The privileges `spec_apply.rs` creates the group role with.
const PRIVILEGED_ROLE_PRIVILEGES: &str = "CREATEDB CREATEROLE NOLOGIN REPLICATION BYPASSRLS";

/// `compute_ctl`'s migrations, in its order. The position is the migration id,
/// and it has to match `compute_tools/src/spec.rs` `handle_migrations` — a test
/// pins the list against the directory.
const MIGRATIONS: [&str; 12] = [
    include_str!("../../compute_tools/src/migrations/0001-add_bypass_rls_to_privileged_role.sql"),
    include_str!("../../compute_tools/src/migrations/0002-alter_roles.sql"),
    include_str!(
        "../../compute_tools/src/migrations/0003-grant_pg_create_subscription_to_privileged_role.sql"
    ),
    include_str!("../../compute_tools/src/migrations/0004-grant_pg_monitor_to_privileged_role.sql"),
    include_str!(
        "../../compute_tools/src/migrations/0005-grant_all_on_tables_to_privileged_role.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0006-grant_all_on_sequences_to_privileged_role.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0007-grant_all_on_tables_with_grant_option_to_privileged_role.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0008-grant_all_on_sequences_with_grant_option_to_privileged_role.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0009-revoke_replication_for_previously_allowed_roles.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0010-grant_snapshot_synchronization_funcs_to_privileged_role.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0011-grant_pg_show_replication_origin_status_to_privileged_role.sql"
    ),
    include_str!(
        "../../compute_tools/src/migrations/0012-grant_pg_signal_backend_to_privileged_role.sql"
    ),
];

const CREATE_PRIVILEGED_ROLE: &str =
    include_str!("../../compute_tools/src/sql/create_privileged_role.sql");

/// The customer's login role and the group role it is a member of.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct CustomerRole {
    pub name: String,
    pub privileged: String,
}

impl CustomerRole {
    /// Both names end up unquoted in DDL, so they are held to a shape that needs
    /// no quoting: `compute-ctl`'s own rule for the group role, plus digits after
    /// the first character for the login.
    pub fn new(name: &str, privileged: &str) -> Result<Self, String> {
        let identifier = |s: &str| {
            !s.is_empty()
                && s.len() < 64
                && s.starts_with(|c: char| c.is_ascii_lowercase() || c == '_')
                && s.chars()
                    .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_')
        };
        if !identifier(name) {
            return Err(format!(
                "CUSTOMER_ROLE {name:?} must be 1-63 lowercase letters, digits and underscores, \
                 not starting with a digit"
            ));
        }
        if !identifier(privileged) {
            return Err(format!(
                "privileged role {privileged:?} is not a plain identifier"
            ));
        }
        if name == "cloud_admin" || name == privileged || name.starts_with("pg_") {
            return Err(format!(
                "CUSTOMER_ROLE {name:?} is reserved: it must not be cloud_admin, the privileged \
                 role ({privileged}) or a pg_ role"
            ));
        }
        Ok(Self {
            name: name.to_string(),
            privileged: privileged.to_string(),
        })
    }

    /// `t` when a role of this name exists but is not a member of the group
    /// role. The bootstrap makes the role and its membership in one transaction,
    /// so such a role predates it: most likely one the customer made while they
    /// were superuser. Converging it would hand whoever knows its password the
    /// group's privileges, and clearing `cloud_admin`'s password would lock out
    /// the clients still using that.
    pub fn conflict_sql(&self) -> String {
        format!(
            "SELECT EXISTS (SELECT FROM pg_catalog.pg_roles r \
             WHERE r.rolname OPERATOR(pg_catalog.=) '{login}' \
             AND NOT EXISTS (SELECT FROM pg_catalog.pg_auth_members m \
             JOIN pg_catalog.pg_roles g ON g.oid OPERATOR(pg_catalog.=) m.roleid \
             WHERE m.member OPERATOR(pg_catalog.=) r.oid \
             AND g.rolname OPERATOR(pg_catalog.=) '{privileged}'))",
            login = self.name,
            privileged = self.privileged,
        )
    }

    /// The whole bootstrap as one multi-statement query, which Postgres runs as a
    /// single implicit transaction: a failure halfway leaves the catalog as it
    /// was, and the retry starts from the top. Every statement is safe to run
    /// again, so it runs on every boot — which is also what carries an endpoint
    /// created before `CUSTOMER_ROLE` onto it on its next render.
    pub fn bootstrap_sql(&self) -> String {
        let priv_role = &self.privileged;
        let login = &self.name;
        let mut sql = String::new();

        sql.push_str(
            &CREATE_PRIVILEGED_ROLE
                .replace("{privileged_role_name}", priv_role)
                .replace("{privileges}", PRIVILEGED_ROLE_PRIVILEGES),
        );
        sql.push('\n');

        // MigrationRunner::prepare_database, verbatim.
        sql.push_str(
            "CREATE SCHEMA IF NOT EXISTS yb_amp_migration;\n\
             CREATE TABLE IF NOT EXISTS yb_amp_migration.migration_id (key pg_catalog.int4 NOT NULL PRIMARY KEY, id pg_catalog.int8 NOT NULL DEFAULT 0);\n\
             INSERT INTO yb_amp_migration.migration_id VALUES (0, 0) ON CONFLICT DO NOTHING;\n\
             ALTER SCHEMA yb_amp_migration OWNER TO cloud_admin;\n\
             REVOKE ALL ON SCHEMA yb_amp_migration FROM PUBLIC;\n",
        );
        // MigrationRunner::run_migrations: each id once, `-- SKIP` ones recorded
        // without running. Several migrations are DO blocks of their own, which
        // EXECUTE runs fine; the distinct dollar tags keep theirs from closing
        // ours.
        for (i, migration) in MIGRATIONS.iter().enumerate() {
            let id = i + 1;
            let body = if migration.starts_with("-- SKIP") {
                String::new()
            } else {
                format!(
                    "EXECUTE $amp_migration_sql${}$amp_migration_sql$; ",
                    migration.replace("{privileged_role_name}", priv_role)
                )
            };
            sql.push_str(&format!(
                "DO $amp_migration$ BEGIN \
                 IF (SELECT id FROM yb_amp_migration.migration_id) < {id} THEN \
                 {body}UPDATE yb_amp_migration.migration_id SET id = {id}; \
                 END IF; END $amp_migration$;\n"
            ));
        }

        // The login, as spec_apply.rs creates a member role — and converged on
        // every boot, so a role of this name that predates the bootstrap (or was
        // made a superuser by hand) is brought back to exactly this.
        sql.push_str(&format!(
            "DO $amp_customer$ BEGIN \
             IF NOT EXISTS (SELECT FROM pg_catalog.pg_roles WHERE rolname OPERATOR(pg_catalog.=) '{login}') THEN \
             CREATE ROLE {login}; \
             END IF; END $amp_customer$;\n\
             ALTER ROLE {login} NOSUPERUSER LOGIN INHERIT CREATEROLE CREATEDB BYPASSRLS REPLICATION;\n\
             GRANT {priv_role} TO {login};\n"
        ));

        // The `postgres` database usable without being its owner. The default
        // privileges are compute_ctl's `default_grants.sql`, covering what
        // cloud_admin creates in `public` (the preloaded extensions' objects).
        sql.push_str(&format!(
            "GRANT CREATE ON DATABASE postgres TO {priv_role};\n\
             DO $amp_public$ BEGIN \
             IF EXISTS (SELECT FROM pg_catalog.pg_namespace WHERE nspname OPERATOR(pg_catalog.=) 'public') THEN \
             GRANT USAGE, CREATE ON SCHEMA public TO {priv_role}; \
             ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO {priv_role} WITH GRANT OPTION; \
             ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO {priv_role} WITH GRANT OPTION; \
             END IF; END $amp_public$;\n"
        ));
        sql
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn role() -> CustomerRole {
        CustomerRole::new("amp_admin", DEFAULT_PRIVILEGED_ROLE).unwrap()
    }

    /// The list is compute_tools' directory, in order. A migration added there
    /// and not here would be run by compute_ctl's runner over ids this already
    /// recorded — or, worse, a reorder would record one migration's id for
    /// another's SQL.
    #[test]
    fn the_migrations_are_compute_tools_own_in_order() {
        let dir = concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../compute_tools/src/migrations"
        );
        let mut files: Vec<_> = std::fs::read_dir(dir)
            .unwrap()
            .filter_map(|e| {
                let p = e.unwrap().path();
                (p.extension()? == "sql").then_some(p)
            })
            .collect();
        files.sort();
        assert_eq!(files.len(), MIGRATIONS.len(), "{files:?}");
        for (path, embedded) in files.iter().zip(MIGRATIONS) {
            assert_eq!(
                std::fs::read_to_string(path).unwrap(),
                embedded,
                "{} is out of place",
                path.display()
            );
        }
    }

    /// The names reach DDL unquoted, so anything that would need quoting — or
    /// that names a role this must never hand the customer — is refused.
    #[test]
    fn only_plain_unreserved_names_are_accepted() {
        assert!(CustomerRole::new("amp_admin", "yb_amp_superuser").is_ok());
        assert!(CustomerRole::new("admin2", "yb_amp_superuser").is_ok());
        for bad in [
            "",
            "Admin",
            "2admin",
            "amp-admin",
            "a\"; DROP ROLE x; --",
            "cloud_admin",
            "yb_amp_superuser",
            "pg_monitor",
            &"a".repeat(64),
        ] {
            assert!(
                CustomerRole::new(bad, "yb_amp_superuser").is_err(),
                "{bad:?} was accepted"
            );
        }
        assert!(CustomerRole::new("amp_admin", "Bad-Name").is_err());
    }

    /// The login must come out a non-superuser member of the group role,
    /// whatever state the catalog was in.
    #[test]
    fn the_login_is_a_non_superuser_member_of_the_group() {
        let sql = role().bootstrap_sql();
        assert!(sql.contains(
            "CREATE ROLE yb_amp_superuser CREATEDB CREATEROLE NOLOGIN REPLICATION BYPASSRLS \
             IN ROLE pg_read_all_data, pg_write_all_data;"
        ));
        assert!(sql.contains(
            "ALTER ROLE amp_admin NOSUPERUSER LOGIN INHERIT CREATEROLE CREATEDB BYPASSRLS REPLICATION;"
        ));
        assert!(sql.contains("GRANT yb_amp_superuser TO amp_admin;"));
        assert!(!sql.contains(" SUPERUSER"), "nothing may grant SUPERUSER");
        assert!(!sql.contains("{privileged_role_name}") && !sql.contains("{privileges}"));
    }

    /// Migrations run once each, under compute_ctl's own bookkeeping, and the
    /// SKIP ones — 0009 among them, which would strip REPLICATION from every role
    /// including the customer — are recorded without being run.
    #[test]
    fn migrations_are_tracked_and_skips_are_not_run() {
        let sql = role().bootstrap_sql();
        for id in 1..=MIGRATIONS.len() {
            assert!(sql.contains(&format!(
                "IF (SELECT id FROM yb_amp_migration.migration_id) < {id} THEN"
            )));
            assert!(sql.contains(&format!("SET id = {id};")));
        }
        assert!(sql.contains("GRANT pg_signal_backend TO yb_amp_superuser WITH ADMIN OPTION"));
        assert!(
            !sql.contains("NOREPLICATION"),
            "migration 0009 must be skipped"
        );
    }

    /// No migration may close the dollar quote its EXECUTE wraps it in.
    #[test]
    fn no_migration_collides_with_the_wrapper_tags() {
        for m in MIGRATIONS {
            assert!(!m.contains("$amp_"), "{m}");
        }
        assert!(!CREATE_PRIVILEGED_ROLE.contains("$amp_"));
    }

    /// Without CREATE on `public`, a fresh customer could not create a table in
    /// the database every amp client connects to.
    #[test]
    fn the_postgres_database_is_usable_by_the_group() {
        let sql = role().bootstrap_sql();
        assert!(sql.contains("GRANT CREATE ON DATABASE postgres TO yb_amp_superuser;"));
        assert!(sql.contains("GRANT USAGE, CREATE ON SCHEMA public TO yb_amp_superuser;"));
    }
}
