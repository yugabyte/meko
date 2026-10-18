import { createHash, createHmac, randomBytes, scryptSync, timingSafeEqual } from 'node:crypto';
import { isIPv6 } from 'node:net';
import { isLoopback } from './config.js';
import { audit, get, kvDelete, kvGet, kvSet, run, type Db } from './db.js';

const SESSION_TTL_MS = 14 * 24 * 3600 * 1000;
const SESSION_RENEW_AFTER_MS = 3600 * 1000;
const SETUP_TOKEN_TTL_MS = 7 * 24 * 3600 * 1000;
export const COOKIE_NAME = 'ak_session';

const sha256 = (s: string) => createHash('sha256').update(s).digest('hex');

// ---- local access ----

const LOCAL_HOSTS = new Set(['localhost', '127.0.0.1', '[::1]']);
/** Headers a reverse proxy adds: the request really came from somewhere else. */
const FORWARDED = ['forwarded', 'x-forwarded-for', 'x-forwarded-host', 'x-real-ip'];
/** `Sec-Fetch-Site` values for a request the user made on this origin (or typed in the address bar). */
const OWN_FETCH = new Set(['same-origin', 'none']);

/**
 * True when a request needs no login: the server listens only on loopback (`bind`), the
 * connection comes from loopback, the Host header names this computer, no proxy says it
 * forwarded the request, and the browser doesn't say another site sent it. The Host check keeps
 * a DNS-rebound page (Host: attacker.example) out. The site checks stand in for the SameSite=Strict
 * cookie: without them, an `<img>` or `<script>` on any page could make logged-in GETs and learn
 * from whether they load. A server listening on the network always asks for a login, even from
 * localhost. A loopback one trusts anything that reaches 127.0.0.1 without those headers: other
 * users of the computer, and tunnels or proxies that forward to it.
 */
/** The host (and port) of an `Origin` header, or null when it isn't a URL (`null`, say). */
function originHost(origin: string): string | null {
  try {
    return new URL(origin).host.toLowerCase();
  } catch {
    return null;
  }
}

export function localAccess(bind: string, remoteAddress: string | undefined, headers: Record<string, string | string[] | undefined>): boolean {
  const host = headers.host;
  if (!isLoopback(bind) || !remoteAddress || typeof host !== 'string') return false;
  if (FORWARDED.some((h) => headers[h] !== undefined)) return false;
  const site = headers['sec-fetch-site'];
  if (site !== undefined && !(typeof site === 'string' && OWN_FETCH.has(site))) return false;
  const origin = headers.origin;
  if (origin !== undefined && !(typeof origin === 'string' && originHost(origin) === host.toLowerCase())) return false;
  const addr = remoteAddress.replace(/^::ffff:/i, '');
  if (!(addr === '::1' || /^127\.\d+\.\d+\.\d+$/.test(addr))) return false;
  try {
    return LOCAL_HOSTS.has(new URL(`http://${host}`).hostname.toLowerCase());
  } catch {
    return false;
  }
}

// ---- passwords (scrypt) ----

const SCRYPT = { N: 1 << 15, r: 8, p: 1, keylen: 64 };

export function hashPassword(password: string): string {
  const salt = randomBytes(16);
  const hash = scryptSync(password, salt, SCRYPT.keylen, { N: SCRYPT.N, r: SCRYPT.r, p: SCRYPT.p, maxmem: 128 * 1024 * 1024 });
  return `scrypt$${SCRYPT.N}$${SCRYPT.r}$${SCRYPT.p}$${salt.toString('base64')}$${hash.toString('base64')}`;
}

export function verifyPassword(password: string, stored: string): boolean {
  if (typeof password !== 'string') return false;
  const [alg, n, r, p, saltB64, hashB64] = stored.split('$');
  if (alg !== 'scrypt') return false;
  const expected = Buffer.from(hashB64, 'base64');
  const got = scryptSync(password, Buffer.from(saltB64, 'base64'), expected.length, {
    N: Number(n), r: Number(r), p: Number(p), maxmem: 128 * 1024 * 1024,
  });
  return timingSafeEqual(expected, got);
}

let dummyHash: string | null = null;

/** Spends the same time as checking a real password, so a wrong username can't be told apart by timing. */
export function burnPasswordCheck(password: unknown): void {
  dummyHash ??= hashPassword('not-the-password');
  verifyPassword(typeof password === 'string' ? password : '', dummyHash);
}

export function validatePassword(password: string): string | null {
  if (typeof password !== 'string') return 'Enter a password.';
  if (password.length < 12) return 'Use at least 12 characters.';
  return null;
}

// ---- owner ----

interface OwnerRow {
  username: string;
  pw_scrypt: string;
  totp_secret: string | null;
  totp_pending: string | null;
}

export function getOwner(db: Db): OwnerRow | undefined {
  return get<OwnerRow>(db, 'SELECT username, pw_scrypt, totp_secret, totp_pending FROM owner WHERE id = 1');
}

/** Creates the owner, or resets it (password, TOTP, all sessions) when one exists. */
export function setOwner(db: Db, username: string, password: string): void {
  run(db, `INSERT INTO owner(id, username, pw_scrypt) VALUES (1, ?, ?)
           ON CONFLICT(id) DO UPDATE SET username = excluded.username, pw_scrypt = excluded.pw_scrypt,
             totp_secret = NULL, totp_pending = NULL`, username, hashPassword(password));
  run(db, 'DELETE FROM auth_sessions');
}

/** Changes the password and signs out every other browser (all sessions but `keepToken`'s). */
export function changePassword(db: Db, current: string, next: string, keepToken?: string): string | null {
  const o = getOwner(db);
  if (!o || !verifyPassword(current, o.pw_scrypt)) return 'Current password is wrong.';
  const bad = validatePassword(next);
  if (bad) return bad;
  run(db, 'UPDATE owner SET pw_scrypt = ? WHERE id = 1', hashPassword(next));
  run(db, 'DELETE FROM auth_sessions WHERE token_sha256 <> ?', keepToken ? sha256(keepToken) : '');
  audit(db, 'password_changed');
  return null;
}

// ---- one-time setup tokens ----

/**
 * Issues a one-time setup link token. Before an owner exists it creates the
 * owner; afterwards it resets the owner's credentials (the recovery path when
 * a password is lost: run `agent-k-local setup-link` on the host).
 */
export function issueSetupToken(db: Db): string {
  const token = randomBytes(24).toString('base64url');
  kvSet(db, 'setup_token', JSON.stringify({ sha256: sha256(token), expiresAt: Date.now() + SETUP_TOKEN_TTL_MS }));
  audit(db, 'setup_token_issued');
  return token;
}

/** The outstanding setup token, or null. An expired or unreadable one is deleted. */
function liveSetupToken(db: Db): { sha256: string; expiresAt: number } | null {
  const raw = kvGet(db, 'setup_token');
  if (!raw) return null;
  try {
    const t = JSON.parse(raw) as { sha256: string; expiresAt: number };
    if (typeof t.sha256 === 'string' && Date.now() < t.expiresAt) return t;
  } catch {
    /* unreadable: drop it below */
  }
  kvDelete(db, 'setup_token');
  return null;
}

/** True when `token` is the outstanding setup token, without using it up. */
export function setupTokenValid(db: Db, token: unknown): boolean {
  if (typeof token !== 'string' || !token) return false;
  const t = liveSetupToken(db);
  return !!t && timingSafeEqual(Buffer.from(t.sha256), Buffer.from(sha256(token)));
}

export function consumeSetupToken(db: Db, token: string): boolean {
  const ok = setupTokenValid(db, token);
  if (ok) kvDelete(db, 'setup_token');
  return ok;
}

/** True while an unexpired setup link exists. */
export function setupTokenOutstanding(db: Db): boolean {
  return liveSetupToken(db) !== null;
}

export function setupPending(db: Db): boolean {
  return !getOwner(db);
}

// ---- cookie sessions ----

export function createAuthSession(db: Db, userAgent: string | undefined, ip: string | undefined): string {
  const token = randomBytes(32).toString('base64url');
  const now = Date.now();
  run(db, 'INSERT INTO auth_sessions(token_sha256, created_at, expires_at, user_agent, ip) VALUES (?, ?, ?, ?, ?)',
    sha256(token), now, now + SESSION_TTL_MS, userAgent ?? null, ip ?? null);
  run(db, 'DELETE FROM auth_sessions WHERE expires_at < ?', now);
  return token;
}

/**
 * Validates a session cookie and slides its expiry forward (at most once an hour). `renewed`
 * says the expiry moved, so the caller can re-issue the cookie with a fresh Max-Age.
 */
export function validateAuthSession(db: Db, token: string | undefined): { valid: boolean; renewed: boolean } {
  if (!authSessionLive(db, token)) return { valid: false, renewed: false };
  const h = sha256(token!);
  const row = get<{ expires_at: number }>(db, 'SELECT expires_at FROM auth_sessions WHERE token_sha256 = ?', h)!;
  const now = Date.now();
  if (row.expires_at - now < SESSION_TTL_MS - SESSION_RENEW_AFTER_MS) {
    run(db, 'UPDATE auth_sessions SET expires_at = ? WHERE token_sha256 = ?', now + SESSION_TTL_MS, h);
    return { valid: true, renewed: true };
  }
  return { valid: true, renewed: false };
}

/** True while a session cookie is signed in, without renewing it. */
export function authSessionLive(db: Db, token: string | undefined): boolean {
  if (!token) return false;
  const row = get<{ expires_at: number }>(db, 'SELECT expires_at FROM auth_sessions WHERE token_sha256 = ?', sha256(token));
  return !!row && row.expires_at >= Date.now();
}

export function deleteAuthSession(db: Db, token: string | undefined): void {
  if (token) run(db, 'DELETE FROM auth_sessions WHERE token_sha256 = ?', sha256(token));
}

export const SESSION_MAX_AGE_S = SESSION_TTL_MS / 1000;

// ---- TOTP (RFC 6238, SHA-1, 30 s, 6 digits) ----

const B32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';

export function base32Encode(buf: Buffer): string {
  let bits = 0, value = 0, out = '';
  for (const byte of buf) {
    value = (value << 8) | byte;
    bits += 8;
    while (bits >= 5) {
      out += B32[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += B32[(value << (5 - bits)) & 31];
  return out;
}

export function base32Decode(s: string): Buffer {
  const clean = s.replace(/=+$/, '').toUpperCase().replace(/\s/g, '');
  let bits = 0, value = 0;
  const out: number[] = [];
  for (const c of clean) {
    const i = B32.indexOf(c);
    if (i < 0) throw new Error('bad base32');
    value = (value << 5) | i;
    bits += 5;
    if (bits >= 8) {
      out.push((value >>> (bits - 8)) & 255);
      bits -= 8;
    }
  }
  return Buffer.from(out);
}

export function totpCode(secret: string, atMs: number = Date.now()): string {
  const counter = Math.floor(atMs / 30000);
  const msg = Buffer.alloc(8);
  msg.writeBigUInt64BE(BigInt(counter));
  const h = createHmac('sha1', base32Decode(secret)).update(msg).digest();
  const off = h[h.length - 1] & 15;
  const bin = ((h[off] & 0x7f) << 24) | (h[off + 1] << 16) | (h[off + 2] << 8) | h[off + 3];
  return String(bin % 1_000_000).padStart(6, '0');
}

export function totpVerify(secret: string, code: string, atMs: number = Date.now()): boolean {
  return totpStep(secret, code, atMs) !== null;
}

/** The 30 s time step a code matches (within one step of drift), or null. */
function totpStep(secret: string, code: unknown, atMs: number): number | null {
  if (typeof code !== 'string') return null;
  const c = code.replace(/\s/g, '');
  if (!/^\d{6}$/.test(c)) return null;
  for (const w of [-1, 0, 1]) if (totpCode(secret, atMs + w * 30000) === c) return Math.floor(atMs / 30000) + w;
  return null;
}

/**
 * Checks a code and uses it up: a code (or an older one) that was already accepted is refused,
 * so a code seen over someone's shoulder can't be replayed within its 90 s window (RFC 6238 §5.2).
 */
export function totpAccept(db: Db, secret: string, code: unknown, atMs: number = Date.now()): boolean {
  const step = totpStep(secret, code, atMs);
  if (step === null) return false;
  // The last step is remembered per secret, so enrolling a new authenticator starts afresh.
  const key = sha256(secret);
  let last = -1;
  try {
    const saved = JSON.parse(kvGet(db, 'totp_last_step') ?? 'null') as { key: string; step: number } | null;
    if (saved?.key === key) last = saved.step;
  } catch {
    /* unreadable: treat as none */
  }
  if (step <= last) return false;
  kvSet(db, 'totp_last_step', JSON.stringify({ key, step }));
  return true;
}

export function newTotpSecret(): string {
  return base32Encode(randomBytes(20));
}

export function otpauthUrl(secret: string, username: string, issuer = 'AgentK Local'): string {
  return `otpauth://totp/${encodeURIComponent(issuer)}:${encodeURIComponent(username)}?secret=${secret}&issuer=${encodeURIComponent(issuer)}`;
}

// ---- login rate limiting (in memory, per client network and site-wide) ----

/**
 * The key failed attempts are counted under. IPv6 clients usually hold a whole /64, so one
 * address per attempt would get around a per-address limit: count per /64 instead. An IPv4
 * address in IPv6 form (`::ffff:1.2.3.4`) counts as the IPv4 address.
 */
export function clientKey(ip: string): string {
  const mapped = /^::ffff:(\d+\.\d+\.\d+\.\d+)$/i.exec(ip);
  if (mapped) return mapped[1];
  if (!isIPv6(ip)) return ip;
  const [head, tail = ''] = ip.split('::');
  const left = head ? head.split(':') : [];
  const right = tail ? tail.split(':') : [];
  // A trailing embedded IPv4 part takes two groups; it is only ever in the low 64 bits.
  const groups = [...left, ...Array(Math.max(0, 8 - left.length - right.length)).fill('0'), ...right];
  return `${groups.slice(0, 4).map((g) => parseInt(g, 16).toString(16)).join(':')}::/64`;
}

export class LoginLimiter {
  private failures = new Map<string, number[]>();
  private all: number[] = [];
  /**
   * `max` failures per client network in the window, and `siteMax` across all clients, so many
   * addresses can't share out a guessing run. Past the site-wide limit everyone waits, including
   * the owner; `agent-k-local setup-link` on the host still gets them in (a valid setup link
   * isn't held back by the limit).
   */
  constructor(private max = 5, private windowMs = 15 * 60 * 1000, private siteMax = 50) {}

  blocked(ip: string, now = Date.now()): boolean {
    this.all = this.all.filter((t) => now - t < this.windowMs);
    return this.recent(clientKey(ip), now).length >= this.max || this.all.length >= this.siteMax;
  }

  fail(ip: string, now = Date.now()): void {
    const key = clientKey(ip);
    this.failures.set(key, [...this.recent(key, now), now]);
    this.all.push(now);
  }

  /** Clears one client's count after it signs in. The site-wide count only runs out. */
  reset(ip: string): void {
    this.failures.delete(clientKey(ip));
  }

  private recent(key: string, now: number): number[] {
    const recent = (this.failures.get(key) ?? []).filter((t) => now - t < this.windowMs);
    if (!recent.length) this.failures.delete(key);
    return recent;
  }
}
