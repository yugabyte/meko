import { describe, expect, it } from 'vitest';
import { flattenPastedEmails, parseEmails, spliceEmails } from '../../src/shared/emails.js';

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

  it('reports a quoted address or a stray bracket instead of dropping or sending it', () => {
    expect(parseEmails('"jane@x.com", b@x.com')).toEqual({ emails: ['b@x.com'], invalid: ['"jane@x.com"'] });
    expect(parseEmails('Jane <jane@x.com')).toEqual({ emails: [], invalid: ['Jane', '<jane@x.com'] });
    expect(parseEmails('jane@x.com>')).toEqual({ emails: [], invalid: ['jane@x.com>'] });
  });
});

describe('spliceEmails', () => {
  it('keeps a paste apart from an address on either side', () => {
    expect(spliceEmails('jane@x.com', 10, 10, 'bob@x.com, c@x.com')).toEqual({ value: 'jane@x.com, bob@x.com, c@x.com', caret: 30 });
    expect(spliceEmails('jane@x.com', 0, 0, 'bob@x.com')).toEqual({ value: 'bob@x.com, jane@x.com', caret: 11 });
    expect(spliceEmails('a@x.com, z@x.com', 9, 9, 'm@x.com')).toEqual({ value: 'a@x.com, m@x.com, z@x.com', caret: 18 });
  });

  it('adds nothing next to a separator or at an empty edge, and replaces the selection', () => {
    expect(spliceEmails('', 0, 0, 'a@x.com')).toEqual({ value: 'a@x.com', caret: 7 });
    expect(spliceEmails('a@x.com;', 8, 8, 'b@x.com')).toEqual({ value: 'a@x.com;b@x.com', caret: 15 });
    expect(spliceEmails('a@x.com old@x.com', 8, 17, 'b@x.com')).toEqual({ value: 'a@x.com b@x.com', caret: 15 });
  });
});

describe('flattenPastedEmails', () => {
  it('turns new lines and tabs into separators', () => {
    expect(flattenPastedEmails('a@x.com\nb@x.com\r\n\r\nc@x.com\td@x.com\n')).toBe('a@x.com, b@x.com, c@x.com, d@x.com');
    expect(flattenPastedEmails('Jane Doe <jane@x.com>\n')).toBe('Jane Doe <jane@x.com>');
  });
});
