import type { DatapackMember, ShareContact } from '../shared/types.js';
import { EMAIL_RE } from '../shared/emails.js';
import { all, run, tx, type Db } from './db.js';

/** Most people kept: recording drops the lowest ranked past this. */
export const MAX_CONTACTS = 200;

const DAY_MS = 24 * 60 * 60 * 1000;

interface ContactRow {
  email_key: string;
  email: string;
  name: string | null;
  shares: number;
  last_at: number;
}

/** Favors people shared with often and lately: a share last month counts half as much as one today. */
function score(c: { shares: number; lastAt: number }, now: number): number {
  return c.shares / (1 + Math.max(0, now - c.lastAt) / (30 * DAY_MS));
}

/** Highest score first, the most recent first among equals. */
export function rankContacts(contacts: ShareContact[], now: number): ShareContact[] {
  return [...contacts].sort((a, b) => score(b, now) - score(a, now) || b.lastAt - a.lastAt);
}

/**
 * The people the Share dialog has invited (`share_contacts`), suggested in it next time: every
 * project's shares in one list, ranked.
 */
export function listContacts(db: Db, now = Date.now()): ShareContact[] {
  const rows = all<ContactRow>(db, 'SELECT * FROM share_contacts');
  return rankContacts(
    rows.filter((r) => EMAIL_RE.test(r.email)).map((r) => ({ email: r.email, name: r.name, shares: r.shares, lastAt: r.last_at })),
    now,
  );
}

/** Counts an invite that went through for each address, keeping its latest spelling, then drops the lowest ranked past MAX_CONTACTS. */
export function recordShares(db: Db, emails: string[], now = Date.now()): void {
  if (!emails.length) return;
  tx(db, () => {
    for (const email of emails) {
      run(db, `INSERT INTO share_contacts(email_key, email, shares, last_at) VALUES (?, ?, 1, ?)
               ON CONFLICT(email_key) DO UPDATE SET email = excluded.email, shares = shares + 1, last_at = excluded.last_at`,
        email.toLowerCase(), email, now);
    }
    for (const c of listContacts(db, now).slice(MAX_CONTACTS)) forgetContact(db, c.email);
  });
}

/** Names the people already kept from a datapack's member list; never adds anyone. */
export function learnNames(db: Db, members: DatapackMember[]): void {
  for (const m of members) {
    if (!m.name || !m.email) continue;
    run(db, 'UPDATE share_contacts SET name = ? WHERE email_key = ? AND name IS NOT ?', m.name, m.email.toLowerCase(), m.name);
  }
}

/** Stops suggesting someone; false when they weren't kept. */
export function forgetContact(db: Db, email: string): boolean {
  return run(db, 'DELETE FROM share_contacts WHERE email_key = ?', email.toLowerCase()).changes > 0;
}
