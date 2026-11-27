/**
 * Suggested changes in review comments, written as GitHub writes them: a code fence whose info
 * string is `suggestion`, holding the exact text to put in place of the lines the comment is on.
 * The Changes panel inserts and draws them, the server explains them to the agent and applies
 * them, and a Review a PR session posts them to GitHub as they are.
 */

/** A part of a comment's body: plain text, or a suggestion's lines (none: delete the lines). */
export type BodyPart = { kind: 'text'; text: string } | { kind: 'suggestion'; lines: string[] };

/** A code fence longer than any run of backticks in the text, and at least three. */
export function fence(text: string): string {
  const longest = Math.max(0, ...(text.match(/`+/g) ?? []).map((m) => m.length));
  return '`'.repeat(Math.max(3, longest + 1));
}

/** A suggestion block holding `text` (lines joined by `\n`; a `\r` before each is dropped). */
export function suggestionBlock(text: string): string {
  const body = text.split('\n').map((l) => l.replace(/\r$/, '')).join('\n');
  const f = fence(body);
  return `${f}suggestion\n${body}${body ? '\n' : ''}${f}`;
}

const OPEN = /^ {0,3}(`{3,})suggestion[ \t]*$/;

/**
 * The body cut into text and suggestion parts. A suggestion opens with a fence of three or more
 * backticks and the info string `suggestion`, and closes with a fence of backticks at least as
 * long; an unclosed one is text, as on GitHub. Other code blocks are text.
 */
export function splitSuggestions(body: string): BodyPart[] {
  const lines = body.split('\n').map((l) => l.replace(/\r$/, ''));
  const parts: BodyPart[] = [];
  let text: string[] = [];
  const flush = () => {
    if (text.length && text.join('\n').trim()) parts.push({ kind: 'text', text: text.join('\n').replace(/^\n+|\n+$/g, '') });
    text = [];
  };
  for (let i = 0; i < lines.length; i++) {
    const open = OPEN.exec(lines[i]);
    const close = open && new RegExp(`^ {0,3}\`{${open[1].length},}[ \\t]*$`);
    const end = close ? lines.findIndex((l, k) => k > i && close.test(l)) : -1;
    if (end < 0) {
      text.push(lines[i]);
      continue;
    }
    flush();
    parts.push({ kind: 'suggestion', lines: lines.slice(i + 1, end) });
    i = end;
  }
  flush();
  return parts;
}

/** The suggestions in a body, each as its lines. */
export function suggestions(body: string): string[][] {
  return splitSuggestions(body).flatMap((p) => (p.kind === 'suggestion' ? [p.lines] : []));
}

export function hasSuggestion(body: string): boolean {
  return suggestions(body).length > 0;
}

/** Excerpt lines over this many characters are stored cut to it plus '…' (`clipExcerptLine` on the server). */
export const EXCERPT_LINE_CHARS = 1000;

const bare = (l: string) => l.replace(/\r$/, '');
const isCut = (l: string) => l.length > EXCERPT_LINE_CHARS && l.endsWith('…');

/**
 * Whether a comment's stored excerpt holds every one of its `count` lines, none cut short: only
 * then can Suggest a change start from it. The server keeps an excerpt's last 20 lines and cuts
 * long ones, so editing a comment on more lines needs them from the diff.
 */
export function excerptWhole(excerpt: string, count: number): boolean {
  const lines = excerpt.split('\n');
  return lines.length === count && !lines.some(isCut);
}

/**
 * Whether a range's lines now (`current`) still end with the comment's stored excerpt: the text
 * its suggestion was written against. A cut excerpt line matches a line that starts with what's
 * left of it; a trailing `\r` is ignored on both sides.
 */
export function excerptMatches(current: string[], excerpt: string): boolean {
  const want = excerpt.split('\n');
  if (want.length > current.length) return false;
  const tail = current.slice(current.length - want.length);
  return want.every((w, k) => {
    const got = bare(tail[k]);
    const line = bare(w);
    return got === line || (isCut(line) && got.startsWith(line.slice(0, -1)));
  });
}
