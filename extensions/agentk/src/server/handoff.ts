import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { parse } from 'smol-toml';
import { legacyOwner } from './auth.js';
import { dbPath, type Config } from './config.js';
import { defaultDeps, heldLease, type InstanceDeps } from './instance.js';
import { expandHome, type Paths } from './paths.js';

/**
 * What `me_koder _handoff --json` prints for Meko (its internal/extensions Handoff): where the server
 * listens, for Meko's web listener to serve at /ext/agentk/, and the web settings and login this
 * install had when it served browsers itself, which Meko takes over with its port. Meko asks at each
 * of its starts and fills in only what it doesn't have yet.
 */
export interface Handoff {
  socket: string;
  legacy: Legacy | null;
}

export interface Legacy {
  bind: string;
  port: number;
  tls: { cert: string; key: string; self_signed: boolean } | null;
  owner: { username: string; pw_scrypt: string; totp_secret: string } | null;
}

/** AgentK's own self-signed pair, made when it served browsers itself over HTTPS. */
export const SELF_SIGNED_CERT = 'self-signed.crt';
export const SELF_SIGNED_KEY = 'self-signed.key';

/**
 * The handoff for this home. `legacy` is there only for an install that served browsers itself: its
 * config.toml names [server] or [tls], it made its own certificate, or it has a login. A new install
 * has none of these, so Meko keeps its own web settings rather than taking AgentK's old defaults.
 */
export function handoff(paths: Paths, cfg: Config, deps?: InstanceDeps): Handoff {
  // The home's server runs on another host (a home shared over NFS that hasn't moved into Meko's
  // data folder): its socket can't be reached from here, and its database is only that server's to
  // open (a WAL reader on another host can't share its -shm index). Meko asks again later.
  const lease = heldLease(paths.lock, deps);
  const here = (deps ?? defaultDeps()).host;
  if (lease && lease.host !== here) {
    throw new Error(`AgentK's server for this home runs on ${lease.host}, not on this host (${here})`);
  }
  const certFile = join(paths.certs, SELF_SIGNED_CERT);
  const keyFile = join(paths.certs, SELF_SIGNED_KEY);
  const configured = !!(cfg.tls.cert && cfg.tls.key);
  const selfSigned = existsSync(certFile) && existsSync(keyFile);
  const owner = readOwner(dbPath(paths, cfg));
  const legacy = namesServer(paths) || configured || selfSigned || !!owner;
  return {
    socket: paths.webSock,
    legacy: legacy
      ? {
        bind: cfg.server.bind,
        port: cfg.server.port,
        tls: configured
          ? { cert: expandHome(cfg.tls.cert), key: expandHome(cfg.tls.key), self_signed: false }
          : selfSigned ? { cert: certFile, key: keyFile, self_signed: true } : null,
        owner: owner ? { username: owner.username, pw_scrypt: owner.pw_scrypt, totp_secret: owner.totp_secret ?? '' } : null,
      }
      : null,
  };
}

/** Whether config.toml has a [server] bind or port: an install from before Meko served AgentK. */
function namesServer(paths: Paths): boolean {
  try {
    const server = parse(readFileSync(paths.config, 'utf8')).server as Record<string, unknown> | undefined;
    return !!server && ('bind' in server || 'port' in server);
  } catch {
    return false;
  }
}

/**
 * The old login, read without changing the database. The server may have it open (on this host,
 * as `handoff` makes sure): a read-only reader there shares its WAL index, so this is the one read
 * outside the server's lease.
 */
function readOwner(file: string): ReturnType<typeof legacyOwner> {
  if (!existsSync(file)) return undefined;
  let db: DatabaseSync | null = null;
  try {
    db = new DatabaseSync(file, { readOnly: true });
    db.exec('PRAGMA busy_timeout = 5000');
    return legacyOwner(db);
  } catch {
    return undefined; // no owner table (a database older than the login), or none at all
  } finally {
    db?.close();
  }
}
