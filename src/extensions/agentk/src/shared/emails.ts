// Email addresses typed or pasted into the Share dialog. Shared by the server and the web UI.

/** No quotes, brackets or separators either: a stray `<` or a quoted address is a typo to report, not an address. */
export const EMAIL_RE = /^[^\s@<>"(),;:]+@[^\s@<>"(),;:]+\.[^\s@<>"(),;:]+$/;

/** How many addresses one invite can send to. */
export const MAX_SHARE_EMAILS = 50;

/**
 * The addresses in a list separated by commas, semicolons or whitespace. An entry copied from a
 * mail client, `Jane Doe <jane@example.com>` or `"Doe, Jane" <jane@example.com>`, counts as the
 * address in `<…>`. Repeats (ignoring case) are dropped, the first spelling kept; anything that
 * isn't an address is in `invalid`, in the order typed.
 */
export function parseEmails(text: string): { emails: string[]; invalid: string[] } {
  const emails: string[] = [];
  const invalid: string[] = [];
  const seen = new Set<string>();
  const add = (token: string) => {
    if (!token) return;
    if (!EMAIL_RE.test(token)) {
      if (!invalid.includes(token)) invalid.push(token);
      return;
    }
    const key = token.toLowerCase();
    if (seen.has(key)) return;
    seen.add(key);
    emails.push(token);
  };
  // A quoted display name (one followed by `<…>`) can hold a comma: drop it before splitting.
  for (const part of text.replace(/"[^"]*"(?=\s*<)/g, ' ').split(/[,;\r\n]+/)) {
    const bracketed = [...part.matchAll(/<([^<>]*)>/g)].map((m) => m[1].trim());
    if (!bracketed.length) {
      part.split(/\s+/).forEach(add);
      continue;
    }
    bracketed.forEach(add);
    // The rest names the person, but an address written next to them still counts.
    part
      .replace(/<[^<>]*>/g, ' ')
      .split(/\s+/)
      .filter((t) => t.includes('@'))
      .forEach(add);
  }
  return { emails, invalid };
}

/**
 * Pasted text for the one-line Emails field: new lines and tabs (a column or row copied from a
 * spreadsheet) become `, `, since a one-line input would otherwise drop or keep them unseen.
 */
export function flattenPastedEmails(text: string): string {
  return text
    .split(/[\r\n\t]+/)
    .map((s) => s.trim())
    .filter(Boolean)
    .join(', ');
}

/**
 * The field after pasting `flat` over `value`'s selection [start, end), with `, ` added at a join
 * whose neighbour isn't already a separator, so it stays apart from an address on either side;
 * `caret` is just after the paste.
 */
export function spliceEmails(value: string, start: number, end: number, flat: string): { value: string; caret: number } {
  const before = value.slice(0, start);
  const after = value.slice(end);
  const join = (c: string) => (c && !/[\s,;]/.test(c) ? ', ' : '');
  const added = join(before.slice(-1)) + flat + join(after.slice(0, 1));
  return { value: before + added + after, caret: before.length + added.length };
}

/**
 * The entry being typed at the end of the Emails field, as `query`, and where it starts: what
 * follows the last comma or semicolon, from its last address, or from the words of a name before
 * it. An address still has no space or separator after it, so a half-typed `bob@x.c` (which looks
 * complete) is still the query. After an address and a separator, or a `Name <address>` entry,
 * nothing is being typed: the query is empty and a pick goes at the end.
 */
export function typedEntry(value: string): { start: number; query: string } {
  const from = Math.max(value.lastIndexOf(','), value.lastIndexOf(';')) + 1;
  const words = [...value.slice(from).matchAll(/\S+/g)].map((m) => ({ text: m[0], at: from + m.index }));
  const none = { start: value.length, query: '' };
  const last = words.at(-1);
  if (!last || /[<>"]/.test(last.text)) return none;
  if (last.text.includes('@')) return /\s$/.test(value) ? none : { start: last.at, query: last.text };
  // Words that aren't addresses: the name being typed, all of them.
  let first = words.length - 1;
  while (first > 0 && !/[@<>"]/.test(words[first - 1].text)) first--;
  return { start: words[first].at, query: value.slice(words[first].at).trim() };
}

/** The field after picking a suggested address: it replaces the entry being typed, and `, ` follows, ready for the next. */
export function pickEmail(value: string, email: string): string {
  const before = value.slice(0, typedEntry(value).start);
  if (!before.trim()) return `${email}, `;
  const join = /\s$/.test(before) ? '' : /[,;]$/.test(before) ? ' ' : ', ';
  return `${before}${join}${email}, `;
}

/**
 * The people to suggest, in the order given: not in `exclude` (lower-case addresses), and, with a
 * query, those whose address holds it or whose name has it at the start of a word (`Jane D` finds
 * Jane Doe), ignoring case.
 */
export function suggestContacts<T extends { email: string; name: string | null }>(
  contacts: T[],
  { exclude, query, limit }: { exclude: Set<string>; query: string; limit: number },
): T[] {
  const q = query.trim().replace(/\s+/g, ' ').toLowerCase();
  const matches = (c: T) =>
    !q || c.email.toLowerCase().includes(q) || ` ${(c.name ?? '').trim().replace(/\s+/g, ' ').toLowerCase()}`.includes(` ${q}`);
  return contacts.filter((c) => !exclude.has(c.email.toLowerCase()) && matches(c)).slice(0, limit);
}
