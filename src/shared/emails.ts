// Email addresses typed or pasted into the Share dialog. Shared by the server and the web UI.

export const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

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
  // A quoted display name can hold a comma: drop it before splitting.
  for (const part of text.replace(/"[^"]*"/g, ' ').split(/[,;\r\n]+/)) {
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
