import { describe, expect, it } from 'vitest';
import { flattenPastedEmails, parseEmails } from '../../src/shared/emails.js';

describe('parseEmails', () => {
  it('splits on commas, semicolons and whitespace', () => {
    expect(parseEmails('a@x.com, b@x.com;c@x.com d@x.com\te@x.com\nf@x.com\r\ng@x.com').emails).toEqual([
      'a@x.com', 'b@x.com', 'c@x.com', 'd@x.com', 'e@x.com', 'f@x.com', 'g@x.com',
    ]);
    expect(parseEmails(' ,; a@x.com ;, ')).toEqual({ emails: ['a@x.com'], invalid: [] });
    expect(parseEmails('')).toEqual({ emails: [], invalid: [] });
    expect(parseEmails('  ,;  ')).toEqual({ emails: [], invalid: [] });
  });

  it('drops repeats ignoring case, keeping the first spelling', () => {
    expect(parseEmails('Bob@X.com bob@x.com, a@x.com BOB@x.com').emails).toEqual(['Bob@X.com', 'a@x.com']);
  });

  it('reports what isn\'t an address, in the order typed and once each', () => {
    expect(parseEmails('a@x.com bob, c@x; bob @x.com')).toEqual({ emails: ['a@x.com'], invalid: ['bob', 'c@x', '@x.com'] });
  });

  it('takes the address in <…> of an entry copied from a mail client', () => {
    expect(parseEmails('Jane Doe <jane@x.com>, Bob <bob@x.com>; carol@x.com').emails).toEqual(['jane@x.com', 'bob@x.com', 'carol@x.com']);
    // A quoted name may hold a comma.
    expect(parseEmails('"Doe, Jane" <jane@x.com>, "Roe, Bob" <bob@x.com>')).toEqual({ emails: ['jane@x.com', 'bob@x.com'], invalid: [] });
    // Several on one line with only spaces between them.
    expect(parseEmails('Jane <jane@x.com> Bob <bob@x.com>').emails).toEqual(['jane@x.com', 'bob@x.com']);
    // An address next to a bracketed one still counts; a bad one in brackets is reported.
    expect(parseEmails('Jane <jane@x.com> a@x.com <nope>')).toEqual({ emails: ['jane@x.com', 'a@x.com'], invalid: ['nope'] });
    // Without brackets, a name is just words that aren't addresses.
    expect(parseEmails('Jane Doe').invalid).toEqual(['Jane', 'Doe']);
  });
});

describe('flattenPastedEmails', () => {
  it('turns new lines and tabs into separators', () => {
    expect(flattenPastedEmails('a@x.com\nb@x.com\r\n\r\nc@x.com\td@x.com\n')).toBe('a@x.com, b@x.com, c@x.com, d@x.com');
    expect(flattenPastedEmails('Jane Doe <jane@x.com>\n')).toBe('Jane Doe <jane@x.com>');
  });
});
