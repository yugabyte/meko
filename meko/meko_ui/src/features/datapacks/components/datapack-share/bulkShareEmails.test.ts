import { describe, it, expect } from "vitest";
import {
  bulkShareStatusPresentation,
  describeBulkShareParse,
  parseBulkShareEmails,
  parseCsvRows,
  summarizeBulkShareResults,
} from "@/features/datapacks/components/datapack-share/bulkShareEmails";

describe("parseCsvRows", () => {
  it("splits simple rows and cells", () => {
    expect(parseCsvRows("a,b\nc,d")).toEqual([
      ["a", "b"],
      ["c", "d"],
    ]);
  });

  it("handles CRLF line endings and a missing trailing newline", () => {
    expect(parseCsvRows("a\r\nb\r\n")).toEqual([["a"], ["b"]]);
    expect(parseCsvRows("a\r\nb")).toEqual([["a"], ["b"]]);
  });

  it("keeps commas, newlines and escaped quotes inside quoted fields", () => {
    expect(parseCsvRows('"Doe, Jane","line1\nline2","say ""hi"""')).toEqual([
      ["Doe, Jane", "line1\nline2", 'say "hi"'],
    ]);
  });

  it("drops blank lines", () => {
    expect(parseCsvRows("a\n\n   \nb")).toEqual([["a"], ["b"]]);
  });
});

describe("parseBulkShareEmails", () => {
  it("returns nothing for empty input", () => {
    expect(parseBulkShareEmails("")).toEqual({
      emails: [],
      skippedRows: [],
      droppedFragments: [],
      duplicateCount: 0,
    });
  });

  it("reads a one-per-line list, trimming and lowercasing", () => {
    const parse = parseBulkShareEmails("  Ana@Company.com \nbo@company.com\n");
    expect(parse.emails).toEqual(["ana@company.com", "bo@company.com"]);
    expect(parse.skippedRows).toEqual([]);
  });

  it("reads comma-, semicolon- and space-separated lists", () => {
    expect(parseBulkShareEmails("a@x.com, b@x.com").emails).toEqual(["a@x.com", "b@x.com"]);
    expect(parseBulkShareEmails("a@x.com; b@x.com").emails).toEqual(["a@x.com", "b@x.com"]);
    expect(parseBulkShareEmails("a@x.com b@x.com").emails).toEqual(["a@x.com", "b@x.com"]);
  });

  it("skips a header row that names an email column", () => {
    const parse = parseBulkShareEmails("name,email,team\nAna,ana@x.com,infra\n");
    expect(parse.emails).toEqual(["ana@x.com"]);
    expect(parse.skippedRows).toEqual([]);
  });

  it("does not treat a bad first line as a header when it looks like data", () => {
    const parse = parseBulkShareEmails("oops\nb@x.com\n");
    expect(parse.emails).toEqual(["b@x.com"]);
    expect(parse.skippedRows).toEqual(["oops"]);
  });

  it("ignores adjacent metadata columns on rows that do contain an address", () => {
    const parse = parseBulkShareEmails('"Doe, Jane",jane@x.com,Maintainer since 2024\n');
    expect(parse.emails).toEqual(["jane@x.com"]);
    expect(parse.skippedRows).toEqual([]);
  });

  it("reports rows with no usable address instead of dropping them", () => {
    const parse = parseBulkShareEmails("a@x.com\nnot-an-email\nAna,infra\n");
    expect(parse.emails).toEqual(["a@x.com"]);
    expect(parse.skippedRows).toEqual(["not-an-email", "Ana, infra"]);
  });

  it("de-duplicates case-insensitively and counts the collapses", () => {
    const parse = parseBulkShareEmails("a@x.com\nA@X.com\na@x.com\nb@x.com");
    expect(parse.emails).toEqual(["a@x.com", "b@x.com"]);
    expect(parse.duplicateCount).toBe(2);
  });

  it("unwraps display-name / angle-bracket forms", () => {
    expect(parseBulkShareEmails('"Ana" <ana@x.com>').emails).toEqual(["ana@x.com"]);
    expect(parseBulkShareEmails("<bo@x.com>,").emails).toEqual(["bo@x.com"]);
  });

  it("surfaces email-looking fragments when a row still had a valid address", () => {
    // "bob@x. com" — space between the period and the TLD. The row
    // still has one valid address (a@x.com), so the row itself isn't
    // skipped; the leftover token that carries an `@` gets surfaced so
    // the user notices the counter dropped from what they typed.
    // `com` on its own has no `@` and isn't a lone-word-with-dot, so
    // it's treated as ambiguous metadata (could be a team name) —
    // one fragment is enough to trigger the amber notice.
    const parse = parseBulkShareEmails("a@x.com bob@x. com");
    expect(parse.emails).toEqual(["a@x.com"]);
    expect(parse.skippedRows).toEqual([]);
    expect(parse.droppedFragments).toEqual(["bob@x"]);
  });

  it("stays quiet on adjacent name / title metadata (not email-looking)", () => {
    // A canonical `Name,email,title` row must not spam the user with
    // "dropped fragment: Ana" — those tokens carry no `@` and don't
    // end with a lone trailing dot, so they're treated as legitimate
    // metadata and silently ignored (same behaviour the parser had
    // before the fragment surfacing landed).
    const parse = parseBulkShareEmails("Ana Smith,ana@x.com,Maintainer since 2024\n");
    expect(parse.emails).toEqual(["ana@x.com"]);
    expect(parse.droppedFragments).toEqual([]);
  });

  it("flags a token that starts with @ (torn-off local part)", () => {
    // If a paste chops the local part off ("@x.com"), the token still
    // has an `@` and should be surfaced — losing the intent silently
    // would be worse than a slightly noisy warning.
    const parse = parseBulkShareEmails("a@x.com @stripped.com");
    expect(parse.emails).toEqual(["a@x.com"]);
    expect(parse.droppedFragments).toEqual(["@stripped.com"]);
  });

  it("round-trips the exported members CSV shape", () => {
    const exported = 'email,name,role,is_owner\r\nana@x.com,Ana,Owner,true\r\n"bo@x.com","Doe, Bo",Viewer,false\r\n';
    expect(parseBulkShareEmails(exported).emails).toEqual(["ana@x.com", "bo@x.com"]);
  });

  // A file saved by Excel as "CSV UTF-8" — or by our own export — begins with a
  // U+FEFF BOM. It is ECMAScript WhiteSpace, so the per-cell trim in
  // isHeaderRow/cellTokens absorbs it; neither the header nor a headerless
  // first address may be lost to the marker.
  it("ignores a leading UTF-8 BOM, with or without a header row", () => {
    const bom = "﻿";
    expect(parseBulkShareEmails(`${bom}email,name\r\nana@x.com,Ana\r\nbo@x.com,Bo\r\n`)).toEqual({
      emails: ["ana@x.com", "bo@x.com"],
      skippedRows: [],
      droppedFragments: [],
      duplicateCount: 0,
    });
    // Headerless single column: the BOM must not swallow the first address.
    expect(parseBulkShareEmails(`${bom}ana@x.com\nbo@x.com\n`)).toEqual({
      emails: ["ana@x.com", "bo@x.com"],
      skippedRows: [],
      droppedFragments: [],
      duplicateCount: 0,
    });
  });
});

describe("describeBulkShareParse", () => {
  it("summarises counts, duplicates and skipped rows", () => {
    expect(
      describeBulkShareParse({ emails: ["a@x.com"], skippedRows: [], duplicateCount: 0 }),
    ).toBe("1 address ready");
    expect(
      describeBulkShareParse({
        emails: ["a@x.com", "b@x.com"],
        skippedRows: ["oops"],
        duplicateCount: 3,
      }),
    ).toBe("2 addresses ready · 3 duplicates removed · 1 row skipped");
  });
});

describe("summarizeBulkShareResults", () => {
  const results = [
    { email: "a@x.com", status: "invited" as const },
    { email: "n@x.com", status: "invited_new_user" as const },
    { email: "b@x.com", status: "already_member" as const },
    { email: "c@x.com", status: "invite_exists" as const },
    { email: "o@x.com", status: "is_owner" as const },
    { email: "oops", status: "invalid_email" as const },
    { email: "e@x.com", status: "error" as const },
  ];

  it("groups rows into invited / unchanged / failed", () => {
    const s = summarizeBulkShareResults(results);
    expect(s.total).toBe(7);
    expect(s.invited).toBe(2);
    expect(s.unchanged).toBe(3);
    expect(s.failed).toBe(2);
    expect(s.newUsers).toBe(1);
  });

  it("recomputes per-status counts from the rows", () => {
    expect(summarizeBulkShareResults(results).byStatus).toMatchObject({
      invited: 1,
      invited_new_user: 1,
      already_member: 1,
    });
  });

  it("handles an empty result list", () => {
    expect(summarizeBulkShareResults([])).toMatchObject({
      total: 0,
      invited: 0,
      unchanged: 0,
      failed: 0,
    });
  });
});

describe("bulkShareStatusPresentation", () => {
  it("maps known statuses to labels and tones", () => {
    expect(bulkShareStatusPresentation("invited").tone).toBe("success");
    expect(bulkShareStatusPresentation("already_member").tone).toBe("neutral");
    expect(bulkShareStatusPresentation("invalid_email").tone).toBe("error");
    expect(bulkShareStatusPresentation("invited_new_user").label).toBe("Invited (new user)");
  });

  it("falls back to the raw value for an unknown status", () => {
    expect(bulkShareStatusPresentation("something_new")).toMatchObject({
      label: "something_new",
      tone: "warning",
    });
  });
});
