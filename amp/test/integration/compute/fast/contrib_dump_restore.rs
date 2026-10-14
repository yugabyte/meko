//! A plain-SQL `pg_dump`, restored with `psql -f`, of a schema built on contrib
//! extensions the compute ships: hstore and earthdistance columns with GIN and
//! GiST indexes, a pgcrypto column default, and a moddatetime trigger.
//!
//! Such a restore runs without `ON_ERROR_STOP`. If an extension is missing, its
//! `CREATE EXTENSION` fails, then every table that uses it fails, and psql
//! still exits 0. So the restore must report no error, and every table must
//! come back with its rows.

use anyhow::bail;

use crate::cluster::{Cluster, step};
use crate::db;
use crate::psql;

const SRC: &str = "contrib_dump_src";
const DST: &str = "contrib_dump_dst";

const SCHEMA: &str = "
    CREATE EXTENSION hstore;
    CREATE EXTENSION cube;
    CREATE EXTENSION earthdistance;
    CREATE EXTENSION pgcrypto;
    CREATE EXTENSION moddatetime;
    CREATE TABLE attrs (id int PRIMARY KEY, h hstore);
    CREATE INDEX attrs_h ON attrs USING gin (h);
    CREATE TABLE places (id int PRIMARY KEY, pos earth);
    CREATE INDEX places_pos ON places USING gist (pos);
    CREATE TABLE secrets (id int PRIMARY KEY, token bytea NOT NULL DEFAULT gen_random_bytes(16));
    CREATE TABLE audited (id int PRIMARY KEY, at timestamp);
    CREATE TRIGGER audited_at BEFORE UPDATE ON audited FOR EACH ROW EXECUTE FUNCTION moddatetime(at);
    INSERT INTO attrs SELECT g, hstore('k', g::text) FROM generate_series(1, 100) g;
    INSERT INTO places SELECT g, ll_to_earth(g % 90, g % 180) FROM generate_series(1, 100) g;
    INSERT INTO secrets (id) SELECT g FROM generate_series(1, 100) g;
    INSERT INTO audited SELECT g, '2000-01-01' FROM generate_series(1, 100) g;
";

/// Asked of the source and of the copy, which must agree.
const COMPARE: [&str; 2] = [
    // coalesce: string_agg over no tables is NULL, which query_text cannot read.
    "SELECT coalesce(string_agg(tablename, ',' ORDER BY tablename), '') FROM pg_tables WHERE schemaname = 'public'",
    "SELECT (SELECT count(*) FROM attrs) || '/' || (SELECT count(*) FROM places) || '/' \
     || (SELECT count(*) FROM secrets) || '/' || (SELECT count(*) FROM audited)",
];

fn conn(c: &Cluster, dbname: &str) -> String {
    format!(
        "host=127.0.0.1 port={} user=cloud_admin dbname={dbname}",
        c.compute_port()
    )
}

pub async fn run(c: &Cluster) -> anyhow::Result<()> {
    let admin = db::compute_conn(c.compute_port());
    drop_databases(&admin).await?;
    let result = dump_and_restore(c, &admin).await;
    // Dropped whether or not the checks passed: later scenarios share this
    // compute. A failed check's error wins over a failed drop.
    let dropped = drop_databases(&admin).await;
    result.and(dropped)
}

async fn drop_databases(admin: &str) -> anyhow::Result<()> {
    // One statement per call: DROP and CREATE DATABASE refuse to run in the
    // implicit transaction a multi-statement batch gets.
    for name in [SRC, DST] {
        db::batch(admin, &format!("DROP DATABASE IF EXISTS {name}")).await?;
    }
    Ok(())
}

async fn dump_and_restore(c: &Cluster, admin: &str) -> anyhow::Result<()> {
    let port = c.compute_port();
    let file = c.repo_dir().join("contrib_dump.sql");

    step("A source database whose tables depend on contrib extensions");
    db::batch(admin, &format!("CREATE DATABASE {SRC}")).await?;
    db::batch(&conn(c, SRC), SCHEMA).await?;

    // pg_dump and psql block, so each runs on a blocking thread: the runner's
    // watchdogs then still fire if one hangs.
    step("pg_dump writes the source database to a file");
    let path = file.clone();
    tokio::task::spawn_blocking(move || psql::dump(port, SRC, &path)).await??;

    step("psql restores the file into an empty database and reports no error");
    db::batch(admin, &format!("CREATE DATABASE {DST}")).await?;
    let path = file.clone();
    let stderr = tokio::task::spawn_blocking(move || psql::restore(port, DST, &path)).await??;
    if stderr.contains("ERROR") {
        bail!(
            "the restore of {} exited 0 but psql reported errors, so it skipped what they name:\n{stderr}",
            file.display()
        );
    }

    step("The copy has every table, with every row");
    for q in COMPARE {
        let want = db::query_text(&conn(c, SRC), q).await?;
        let got = db::query_text(&conn(c, DST), q).await?;
        if got != want {
            bail!("the restored database answers {got:?} where the source answers {want:?}: {q}");
        }
    }
    Ok(())
}
