import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { dbPath, loadConfig } from '../../src/server/config.js';
import { openDb, run } from '../../src/server/db.js';
import { handoff, SELF_SIGNED_CERT, SELF_SIGNED_KEY, type Legacy } from '../../src/server/handoff.js';
import { getPaths, type Paths } from '../../src/server/paths.js';
import { KODE_PATH } from '../../src/shared/mekourls.js';

describe('the handoff to Meko', () => {
  let home: string;
  let paths: Paths;
  beforeEach(() => {
    home = mkdtempSync(join(tmpdir(), 'ak-handoff-'));
    paths = getPaths(home);
  });
  afterEach(() => rmSync(home, { recursive: true, force: true }));

  const config = (p: Paths, toml: string) => writeFileSync(p.config, toml);
  const selfSigned = (p: Paths) => {
    mkdirSync(p.certs, { recursive: true });
    for (const f of [SELF_SIGNED_CERT, SELF_SIGNED_KEY]) writeFileSync(join(p.certs, f), 'pem');
  };
  const DEFAULTS = { bind: '0.0.0.0', port: 7777, tls: null, owner: null };

  it.each<[string, (p: Paths) => void, (p: Paths) => Legacy | null]>([
    ['a fresh home', () => undefined, () => null],
    // A config.toml that names neither [server] nor [tls] isn't from a version that served browsers.
    ['other settings only', (p) => config(p, '[repos]\ndepth = 2\n'), () => null],
    ['[server] bind and port', (p) => config(p, '[server]\nbind = "127.0.0.1"\nport = 8443\n'), () => ({ ...DEFAULTS, bind: '127.0.0.1', port: 8443 })],
    ['its own self-signed certificate', selfSigned,
      (p) => ({ ...DEFAULTS, tls: { cert: join(p.certs, SELF_SIGNED_CERT), key: join(p.certs, SELF_SIGNED_KEY), self_signed: true } })],
    // A certificate config.toml names wins over the self-signed one.
    ['[tls] cert and key', (p) => {
      selfSigned(p);
      config(p, '[tls]\ncert = "/etc/agentk/cert.pem"\nkey = "/etc/agentk/key.pem"\n');
    }, () => ({ ...DEFAULTS, tls: { cert: '/etc/agentk/cert.pem', key: '/etc/agentk/key.pem', self_signed: false } })],
    ['a login', (p) => {
      const db = openDb(dbPath(p, loadConfig(p, () => undefined)));
      run(db, 'INSERT INTO owner(id, username, pw_scrypt, totp_secret) VALUES (1, ?, ?, NULL)', 'me', 'scrypt$salt$hash');
      db.close();
    }, () => ({ ...DEFAULTS, owner: { username: 'me', pw_scrypt: 'scrypt$salt$hash', totp_secret: '' } })],
  ])('hands over the socket, and the old web settings and login only for an install that had them: %s', (_name, setup, legacy) => {
    setup(paths);
    expect(handoff(paths, loadConfig(paths, () => undefined))).toEqual({ socket: paths.webSock, path: KODE_PATH, legacy: legacy(paths) });
  });

  it("refuses while the home's server runs on another host, whose socket and database aren't this host's to use", () => {
    const now = Date.now();
    const lease = { host: 'other-host', pid: 1, version: '1', port: 0, startedAt: now, beatAt: now };
    writeFileSync(paths.lock, JSON.stringify(lease));
    const deps = { host: 'this-host', pid: 2, now: () => now, alive: () => true, sleep: async () => undefined };
    expect(() => handoff(paths, loadConfig(paths, () => undefined), deps)).toThrow(/runs on other-host/);
    // One on this host is fine: the socket is here, and a read-only reader shares its WAL index.
    writeFileSync(paths.lock, JSON.stringify({ ...lease, host: 'this-host' }));
    expect(handoff(paths, loadConfig(paths, () => undefined), deps).socket).toBe(paths.webSock);
  });
});
