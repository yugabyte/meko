import { createHash, createHmac, randomBytes, scryptSync, timingSafeEqual } from 'node:crypto';
import { audit, get, kvDelete, kvGet, kvSet, run, type Db } from './db.js';

const SESSION_TTL_MS = 14 * 24 * 3600 * 1000;
const SESSION_RENEW_AFTER_MS = 3600 * 1000;
const SETUP_TOKEN_TTL_MS = 7 * 24 * 3600 * 1000;
export const COOKIE_NAME = 'ak_session';

const sha256 = (s: string) => createHash('sha256').update(s).digest('hex');

// ---- passwords (scrypt) ----

const SCRYPT = { N: 1 << 15, r: 8, p: 1, keylen: 64 };

export function hashPassword(password: string): string {
  const salt = randomBytes(16);
  const hash = scryptSync(password, salt, SCRYPT.keylen, { N: SCRYPT.N, r: SCRYPT.r, p: SCRYPT.p, maxmem: 128 * 1024 * 1024 });
  return `scrypt$${SCRYPT.N}$${SCRYPT.r}$${SCRYPT.p}$${salt.toString('base64')}$${hash.toString('base64')}`;
}

export function verifyPassword(password: string, stored: string): boolean {
  const [alg, n, r, p, saltB64, hashB64] = stored.split('$');
  if (alg !== 'scrypt') return false;
  const expected = Buffer.from(hashB64, 'base64');
  const got = scryptSync(password, Buffer.from(saltB64, 'base64'), expected.length, {
    N: Number(n), r: Number(r), p: Number(p), maxmem: 128 * 1024 * 1024,
  });
  return timingSafeEqual(expected, got);
}

export function validatePassword(password: string): string | null {
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

export function changePassword(db: Db, current: string, next: string): string | null {
  const o = getOwner(db);
  if (!o || !verifyPassword(current, o.pw_scrypt)) return 'Current password is wrong.';
  const bad = validatePassword(next);
  if (bad) return bad;
  run(db, 'UPDATE owner SET pw_scrypt = ? WHERE id = 1', hashPassword(next));
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

export function consumeSetupToken(db: Db, token: string): boolean {
  const raw = kvGet(db, 'setup_token');
  if (!raw || !token) return false;
  const { sha256: want, expiresAt } = JSON.parse(raw) as { sha256: string; expiresAt: number };
  const ok = Date.now() < expiresAt && timingSafeEqual(Buffer.from(want), Buffer.from(sha256(token)));
  if (ok) kvDelete(db, 'setup_token');
  return ok;
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

/** Validates a session cookie and slides its expiry forward. */
export function validateAuthSession(db: Db, token: string | undefined): boolean {
  if (!token) return false;
  const h = sha256(token);
  const row = get<{ expires_at: number }>(db, 'SELECT expires_at FROM auth_sessions WHERE token_sha256 = ?', h);
  const now = Date.now();
  if (!row || row.expires_at < now) return false;
  if (row.expires_at - now < SESSION_TTL_MS - SESSION_RENEW_AFTER_MS) {
    run(db, 'UPDATE auth_sessions SET expires_at = ? WHERE token_sha256 = ?', now + SESSION_TTL_MS, h);
  }
  return true;
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
  const c = code.replace(/\s/g, '');
  if (!/^\d{6}$/.test(c)) return false;
  return [-1, 0, 1].some((w) => totpCode(secret, atMs + w * 30000) === c);
}

export function newTotpSecret(): string {
  return base32Encode(randomBytes(20));
}

export function otpauthUrl(secret: string, username: string, issuer = 'agent-k-local'): string {
  return `otpauth://totp/${encodeURIComponent(issuer)}:${encodeURIComponent(username)}?secret=${secret}&issuer=${encodeURIComponent(issuer)}`;
}

// ---- login rate limiting (in memory, per IP) ----

export class LoginLimiter {
  private failures = new Map<string, number[]>();
  constructor(private max = 5, private windowMs = 15 * 60 * 1000) {}

  blocked(ip: string, now = Date.now()): boolean {
    return this.recent(ip, now).length >= this.max;
  }

  fail(ip: string, now = Date.now()): void {
    this.failures.set(ip, [...this.recent(ip, now), now]);
  }

  reset(ip: string): void {
    this.failures.delete(ip);
  }

  private recent(ip: string, now: number): number[] {
    return (this.failures.get(ip) ?? []).filter((t) => now - t < this.windowMs);
  }
}
