import { describe, expect, it } from 'vitest';
import { MAX_CONTACTS, forgetContact, learnNames, listContacts, recordShares } from '../../src/server/contacts.js';
import { openDb } from '../../src/server/db.js';

const DAY = 24 * 60 * 60 * 1000;

describe('share contacts', () => {
  it('counts each invite, keeps the latest spelling, and names only the people it keeps', () => {
    const db = openDb(':memory:');
    recordShares(db, ['Jane@x.com', 'bob@x.com'], 1000);
    recordShares(db, ['jane@x.com'], 2000);
    learnNames(db, [
      { name: 'Jane Doe', email: 'JANE@x.com', grant: 'contributor' },
      { name: 'Someone', email: 'other@x.com', grant: 'viewer' },
      { name: '', email: 'bob@x.com', grant: 'viewer' },
    ]);
    expect(listContacts(db, 2000)).toEqual([
      { email: 'jane@x.com', name: 'Jane Doe', shares: 2, lastAt: 2000 },
      { email: 'bob@x.com', name: null, shares: 1, lastAt: 1000 },
    ]);
    expect(forgetContact(db, 'BOB@x.com')).toBe(true);
    expect(forgetContact(db, 'bob@x.com')).toBe(false);
    expect(listContacts(db, 2000).map((c) => c.email)).toEqual(['jane@x.com']);
  });

  it('ranks people shared with often and lately first, the most recent among equals', () => {
    const db = openDb(':memory:');
    const now = 400 * DAY;
    const share = (email: string, times: number, daysAgo: number) => {
      for (let i = 0; i < times; i++) recordShares(db, [email], now - daysAgo * DAY);
    };
    share('old-often@x.com', 8, 365);
    share('lately@x.com', 3, 7);
    // The same score: twice a month ago, and once today.
    share('twice-a-month-ago@x.com', 2, 30);
    share('today@x.com', 1, 0);
    expect(listContacts(db, now).map((c) => c.email)).toEqual(['lately@x.com', 'today@x.com', 'twice-a-month-ago@x.com', 'old-often@x.com']);
  });

  it(`keeps the ${MAX_CONTACTS} ranked highest`, () => {
    const db = openDb(':memory:');
    recordShares(db, ['keep@x.com'], 0);
    recordShares(db, ['keep@x.com'], 0);
    recordShares(db, Array.from({ length: MAX_CONTACTS }, (_, i) => `p${i}@x.com`), 0);
    const kept = listContacts(db, 0);
    expect(kept).toHaveLength(MAX_CONTACTS);
    expect(kept[0].email).toBe('keep@x.com');
  });
});
