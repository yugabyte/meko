import { get, type Db } from './db.js';

/**
 * The login AgentK had when it served browsers itself, before Meko took over its port and sign-in.
 * Meko imports it once, through `me_kode _handoff --json` (handoff.ts), so the same password and
 * authenticator still sign in. Nothing else reads it: the table stays only for that import.
 */
export interface LegacyOwner {
  username: string;
  pw_scrypt: string;
  totp_secret: string | null;
}

export function legacyOwner(db: Db): LegacyOwner | undefined {
  return get<LegacyOwner>(db, 'SELECT username, pw_scrypt, totp_secret FROM owner WHERE id = 1');
}
