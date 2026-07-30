import pg from "pg";
import fs from "fs";

const { Pool } = pg;

function sslConfig(): pg.PoolConfig["ssl"] {
  const sslEnabled = process.env.YUGABYTEDB_SSL === "true";
  if (!sslEnabled) return undefined;

  const rejectUnauthorized = process.env.YUGABYTEDB_SSL_REJECT_UNAUTHORIZED !== "false";
  const caPath = process.env.YUGABYTEDB_SSL_CA_CERT;

  return {
    rejectUnauthorized,
    ...(caPath ? { ca: fs.readFileSync(caPath).toString() } : {}),
  };
}

export const pool = new Pool({
  host: process.env.YUGABYTEDB_HOST ?? "127.0.0.1",
  port: parseInt(process.env.YUGABYTEDB_PORT ?? "5433", 10),
  user: process.env.YUGABYTEDB_USER ?? "yugabyte",
  password: process.env.YUGABYTEDB_PASSWORD ?? "yugabyte",
  database: process.env.YUGABYTEDB_DB ?? "yugabyte",
  ssl: sslConfig(),
  max: 5,
  idleTimeoutMillis: 30_000,
  connectionTimeoutMillis: 5_000,
});

pool.on("error", (err) => {
  console.error("Unexpected DB pool error:", err);
});
