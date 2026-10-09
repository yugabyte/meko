// Pure helpers for the bulk "share with a group" flow: turn a pasted list or an
// uploaded CSV into a clean, de-duplicated address list, and turn the server's
// per-row results into UI copy.
//
// Address validity is delegated to parseSingleInviteEmail so the single-invite
// field and the bulk flow can never disagree about what an email looks like.
import type { DatapackBulkShareResult, DatapackBulkShareStatus } from "@/lib/api/mekoApi";
import { parseSingleInviteEmail } from "@/features/datapacks/components/datapack-share/shareInviteEmail";

/**
 * Split CSV text into rows of cells (RFC 4180-ish).
 *
 * Handles quoted fields (`"a,b"`), escaped quotes inside them (`""`), embedded
 * newlines inside quotes, and CRLF vs LF line endings. Trailing empty rows are
 * dropped. Cells are returned verbatim (untrimmed) — callers trim, because
 * whitespace is only ever noise for our use case but not for a generic parser.
 */
export function parseCsvRows(text: string): string[][] {
  const rows: string[][] = [];
  let row: string[] = [];
  let cell = "";
  let inQuotes = false;

  const endCell = () => {
    row.push(cell);
    cell = "";
  };
  const endRow = () => {
    endCell();
    rows.push(row);
    row = [];
  };

  for (let i = 0; i < text.length; i += 1) {
    const ch = text[i];
    if (inQuotes) {
      if (ch === '"') {
        if (text[i + 1] === '"') {
          cell += '"';
          i += 1;
        } else {
          inQuotes = false;
        }
      } else {
        cell += ch;
      }
      continue;
    }
    if (ch === '"') {
      inQuotes = true;
      continue;
    }
    if (ch === ",") {
      endCell();
      continue;
    }
    if (ch === "\r") {
      // Swallow CR; the following LF (or a lone CR) ends the row.
      if (text[i + 1] === "\n") i += 1;
      endRow();
      continue;
    }
    if (ch === "\n") {
      endRow();
      continue;
    }
    cell += ch;
  }
  // Flush whatever is buffered; a file with no trailing newline still has a row.
  if (cell.length > 0 || row.length > 0) endRow();

  // Blank lines produce [""] rows — drop rows with no content at all.
  return rows.filter((r) => r.some((c) => c.trim().length > 0));
}

/** True when a row looks like a CSV header (no address in it, and it names an email column). */
function isHeaderRow(cells: string[]): boolean {
  const trimmed = cells.map((c) => c.trim());
  if (trimmed.some((c) => parseSingleInviteEmail(c).ok)) return false;
  return trimmed.some((c) => /^"?e[-_ ]?mail/i.test(c) || /e[-_ ]?mail(\s*address)?$/i.test(c));
}

/** Candidate tokens in a cell: a cell may itself hold `a@x.com; b@x.com` or space-separated addresses. */
function cellTokens(cell: string): string[] {
  return (
    cell
      .split(/[;\s]+/)
      .map((t) => t.trim())
      // Strip `<a@x.com>` / `"Name" <a@x.com>` wrappers and trailing punctuation.
      .map((t) => t.replace(/^[<("']+/, "").replace(/[>)"',.]+$/, ""))
      .filter((t) => t.length > 0)
  );
}

export type BulkShareEmailParse = {
  /** Unique, lowercased addresses in first-seen order. */
  emails: string[];
  /**
   * Input rows that yielded no usable address, rendered as a single readable
   * string each. Surfaced in the UI so nothing is dropped silently.
   */
  skippedRows: string[];
  /**
   * Tokens that failed the single-email regex on a row that still had at least
   * one valid address. Kept separate from `skippedRows` because the row itself
   * wasn't skipped — the tokens were. Common triggers are typos where the user
   * accidentally inserted a space inside an address (`bob@x. com` becomes two
   * tokens, `bob@x.` and `com`, both dropped); surfacing them lets the user
   * notice the counter is 1 instead of 2 and fix the typo in place.
   */
  droppedFragments: string[];
  /** How many repeat addresses were collapsed (case-insensitive). */
  duplicateCount: number;
};

/**
 * Parse pasted text or an uploaded CSV into addresses.
 *
 * Row-level rule: if a row contains at least one valid address, that row's
 * remaining cells are treated as adjacent metadata (name, team, role…) and
 * ignored. If a row contains NO valid address it is reported in `skippedRows`
 * — a bad row is never silently discarded.
 *
 * A leading header row is skipped only when it clearly looks like a header
 * (contains no address AND names an email column), so a genuine typo on line 1
 * is still reported rather than swallowed.
 *
 * A leading UTF-8 BOM (files exported from here, or saved by Excel as
 * "CSV UTF-8") needs no special handling: U+FEFF is ECMAScript WhiteSpace, so
 * the per-cell `trim()` in isHeaderRow and the `/[;\s]+/` split in cellTokens
 * both absorb it. Keep those trims if you touch this — see the BOM round-trip
 * tests in shareMemberCsv.test.ts and bulkShareEmails.test.ts.
 */
export function parseBulkShareEmails(text: string): BulkShareEmailParse {
  const rows = parseCsvRows(text ?? "");
  const emails: string[] = [];
  const seen = new Set<string>();
  const skippedRows: string[] = [];
  const droppedFragments: string[] = [];
  let duplicateCount = 0;

  rows.forEach((cells, index) => {
    if (index === 0 && isHeaderRow(cells)) return;

    const tokens = cells.flatMap(cellTokens);
    const valid: string[] = [];
    const invalid: string[] = [];
    for (const t of tokens) {
      if (parseSingleInviteEmail(t).ok) {
        valid.push(t);
      } else {
        invalid.push(t);
      }
    }

    if (valid.length === 0) {
      const label = cells
        .map((c) => c.trim())
        .filter((c) => c.length > 0)
        .join(", ");
      if (label) skippedRows.push(label);
      return;
    }

    // Row had at least one valid address. Surface any leftover invalid
    // tokens (typos like "bob@x. com" that split into "bob@x." and
    // "com") so they aren't silently dropped — pushing them onto
    // droppedFragments makes them visible in the UI's amber line.
    // Filter out tokens that look like adjacent metadata a user might
    // legitimately include on the same row: "Ana Smith" (a display
    // name) or "PhD" (a title) don't contain `@` or `.`, so we treat
    // them as name-column noise and quietly ignore them. Only tokens
    // that LOOK LIKE they were trying to be an email — has an `@` or
    // ends in `.` after a word — count as a fragment worth flagging.
    for (const token of invalid) {
      if (looksLikeIntendedEmail(token)) droppedFragments.push(token);
    }

    for (const raw of valid) {
      const email = raw.toLowerCase();
      if (seen.has(email)) {
        duplicateCount += 1;
        continue;
      }
      seen.add(email);
      emails.push(email);
    }
  });

  return { emails, skippedRows, droppedFragments, duplicateCount };
}

/**
 * True when a failed token looks like the user was trying to type an email
 * (so it's worth flagging as a dropped fragment) rather than plain name /
 * team metadata. The intent is to keep the UI quiet on legitimate CSV rows
 * like `Ana Smith,ana@x.com,PhD` while catching the "space inside an
 * address" typo (`bob@x. com` → tokens `bob@x.` and `com`).
 *
 * Heuristic:
 *  - Contains an `@`: almost certainly meant to be an email — flag it.
 *  - Ends with a `.` right after word characters, and no whitespace inside:
 *    looks like the first half of a torn-in-half address (`bob@x.`
 *    without the TLD, or `x.` after the `@` was already tokenized away) —
 *    flag it too.
 *  - Everything else: silent.
 */
function looksLikeIntendedEmail(token: string): boolean {
  if (!token) return false;
  if (token.includes("@")) return true;
  if (/^[\w+.-]+\.$/.test(token)) return true;
  return false;
}

/** Human-readable one-liner describing a parse, for the preview line under the input. */
export function describeBulkShareParse(parse: BulkShareEmailParse): string {
  const parts = [
    `${parse.emails.length} ${parse.emails.length === 1 ? "address" : "addresses"} ready`,
  ];
  if (parse.duplicateCount > 0) {
    parts.push(`${parse.duplicateCount} duplicate${parse.duplicateCount === 1 ? "" : "s"} removed`);
  }
  if (parse.skippedRows.length > 0) {
    parts.push(
      `${parse.skippedRows.length} row${parse.skippedRows.length === 1 ? "" : "s"} skipped`,
    );
  }
  return parts.join(" · ");
}

/** How a per-row status should read in the results table. */
export type BulkShareStatusTone = "success" | "neutral" | "warning" | "error";

const STATUS_PRESENTATION: Record<
  DatapackBulkShareStatus,
  { label: string; tone: BulkShareStatusTone; detail: string }
> = {
  invited: { label: "Invite sent", tone: "success", detail: "Invite email sent." },
  invited_new_user: {
    label: "Invited (new user)",
    tone: "success",
    // The not-yet-provisioned case: no Meko/Cognito account exists yet, so the
    // person has to sign up before the grant takes effect. Called out explicitly
    // rather than folded into "Invite sent".
    detail: "No Meko account yet — they must sign up to accept.",
  },
  already_member: { label: "Already a member", tone: "neutral", detail: "Already has access." },
  invite_exists: {
    label: "Invite pending",
    tone: "neutral",
    detail: "An invite is already pending.",
  },
  is_owner: { label: "Owner", tone: "neutral", detail: "Owns this datapack." },
  invalid_email: { label: "Invalid email", tone: "error", detail: "Not a valid email address." },
  error: { label: "Failed", tone: "error", detail: "Could not be processed." },
};

/** Label + tone + fallback detail for a status; unknown values degrade to the raw string. */
export function bulkShareStatusPresentation(status: string): {
  label: string;
  tone: BulkShareStatusTone;
  detail: string;
} {
  return (
    STATUS_PRESENTATION[status as DatapackBulkShareStatus] ?? {
      label: status,
      tone: "warning",
      detail: "",
    }
  );
}

/** Statuses that mean "this address now has access, or has been asked to accept". */
const GRANTED_STATUSES: DatapackBulkShareStatus[] = ["invited", "invited_new_user"];
/** Statuses that mean "nothing to do" — the idempotent no-ops. */
const UNCHANGED_STATUSES: DatapackBulkShareStatus[] = [
  "already_member",
  "invite_exists",
  "is_owner",
];

export type BulkShareSummary = {
  total: number;
  invited: number;
  unchanged: number;
  failed: number;
  /** Subset of `invited` with no Meko account yet. */
  newUsers: number;
  /** Per-status counts recomputed from the rows (never trusts the server's `summary`). */
  byStatus: Record<string, number>;
};

/** Roll per-row results up into the counts shown above the results table. */
export function summarizeBulkShareResults(results: DatapackBulkShareResult[]): BulkShareSummary {
  const byStatus: Record<string, number> = {};
  for (const r of results) {
    byStatus[r.status] = (byStatus[r.status] ?? 0) + 1;
  }
  const count = (statuses: DatapackBulkShareStatus[]) =>
    statuses.reduce((sum, s) => sum + (byStatus[s] ?? 0), 0);

  const invited = count(GRANTED_STATUSES);
  const unchanged = count(UNCHANGED_STATUSES);
  return {
    total: results.length,
    invited,
    unchanged,
    failed: results.length - invited - unchanged,
    newUsers: byStatus.invited_new_user ?? 0,
    byStatus,
  };
}
