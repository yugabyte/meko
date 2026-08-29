// Bundled "org coding standards" KB doc for the ADLC Review Agent Lab.
// Seeded into the lab's datapack via the same upload-dialog flow
// WorkbenchLMKnowledgePanel already drives for its own sample file (see
// data/sampleKnowledge.ts) — inlined as a string so we can synthesize a
// File on demand without a public-asset fetch.

export const ORG_CODING_STANDARDS_FILENAME = "org-coding-standards.md";

export const ORG_CODING_STANDARDS_CONTENT = `# Org Coding Standards (excerpt)

## SQL

- Never build a SQL string via concatenation or f-strings with request input.
  Always use parameterized queries / prepared statements.
- Every query touching a multi-tenant table must be scoped by the caller's
  tenant/team id, validated against the authenticated session — never trust
  a client-supplied id alone.

## Error handling

- Never use a bare \`except:\` (or \`except Exception:\` without re-raising or
  logging). Catch the specific exception you expect and handle it, or let
  it propagate with context.
- User-facing error responses must not leak internal details (stack traces,
  SQL fragments, file paths).

## Input validation

- Validate and type-check every request parameter before use. Reject
  missing/malformed input with a 4xx before touching the database.

## Security

- New endpoints must be reviewed against the security-compliance checklist
  before merge — auth requirements, rate limiting, and PII handling.
`;

export function buildOrgCodingStandardsFile(): File {
  return new File([ORG_CODING_STANDARDS_CONTENT], ORG_CODING_STANDARDS_FILENAME, {
    type: "text/markdown",
  });
}
