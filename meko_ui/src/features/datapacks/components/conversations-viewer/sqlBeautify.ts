// Tiny, dependency-free SQL beautifier for the trace drawer's "View
// queries" list. We deliberately avoid a library (e.g. sql-formatter is
// ~780KB unpacked / a full multi-dialect lexer) — these statements only
// need light, predictable reflow for readability, not a full parser.
//
// Behaviour:
//  - SQL: normalize whitespace, then break before major clauses / JOINs /
//    boolean sub-conditions so single-line statements (e.g. the bulk
//    UPDATE) and multi-line ones render consistently.
//  - Cypher: left as-is (only dedented) since the SQL rules don't apply.
//  - Bind placeholders (%s / $1) are preserved verbatim.
//  - Best-effort: any failure falls back to the dedented original.

/** Strip the common leading indentation from a multi-line block. */
export function dedent(text: string): string {
  const lines = text.replace(/\t/g, "  ").split("\n");
  const indents = lines
    .filter((l) => l.trim().length > 0)
    .map((l) => (l.match(/^ */)?.[0].length ?? 0));
  const min = indents.length ? Math.min(...indents) : 0;
  return lines.map((l) => l.slice(min)).join("\n").trim();
}

// Top-level SQL clauses that start a new line. Multi-word entries are
// matched with flexible whitespace. Ordered so longer phrases win.
const SQL_CLAUSES = [
  "WITH", "SELECT", "FROM", "WHERE", "GROUP BY", "ORDER BY", "HAVING",
  "LIMIT", "OFFSET", "INSERT INTO", "VALUES", "UPDATE", "SET",
  "DELETE FROM", "RETURNING", "ON CONFLICT DO UPDATE", "ON CONFLICT",
  "UNION ALL", "UNION",
];

/**
 * Beautify a single SQL / Cypher statement for display.
 *
 * @param name  The span name (used only to detect Cypher vs SQL).
 * @param statement  The raw statement text.
 */
export function beautifyStatement(name: string, statement: string): string {
  const raw = statement.trim();
  if (!raw) return raw;
  if (/cypher/i.test(name)) return dedent(raw);
  try {
    let s = raw.replace(/\s+/g, " ").trim();
    for (const kw of SQL_CLAUSES) {
      const re = new RegExp(`\\s+(${kw.replace(/ /g, "\\s+")})\\b`, "gi");
      s = s.replace(re, `\n${kw}`);
    }
    // Keep a JOIN modifier (LEFT/RIGHT/INNER/FULL/CROSS, OUTER) on the
    // same line as JOIN.
    s = s.replace(
      /\s+((?:LEFT|RIGHT|INNER|FULL|CROSS)\s+)?(?:OUTER\s+)?JOIN\b/gi,
      (m) => `\n${m.trim().replace(/\s+/g, " ").toUpperCase()}`,
    );
    // Indent boolean sub-conditions under their clause.
    s = s.replace(/\s+(AND|OR)\b/gi, (_m, k: string) => `\n  ${k.toUpperCase()}`);
    // Uppercase a leading clause keyword if the statement starts with one.
    s = s.replace(/^\s*([A-Za-z]+(?:\s+[A-Za-z]+)?)/, (m) =>
      SQL_CLAUSES.includes(m.trim().toUpperCase()) ? m.toUpperCase() : m,
    );
    return s.trim();
  } catch {
    return dedent(raw);
  }
}
