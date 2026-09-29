// AgentK's own output on yugabyte/yugabyte-db, read from GitHub. Real pull
// requests, real comments, real replies from the engineers who got them.
//
// A CAPTURE, not a live fetch. Nothing in the lab queries GitHub for these:
// the repo is public, so every row deep-links to the thing it describes and a
// reader can check any of it. Same contract as productionSnapshot.ts, and the
// same obligation: move PUBLIC_REVIEWS_READ_ON when you refresh the figures,
// and leave a figure out rather than guessing it.
//
// Refreshing:
//   gh search prs --repo yugabyte/yugabyte-db --commenter "yb-agentk-dev[bot]"
//   gh api "repos/yugabyte/yugabyte-db/pulls/<n>/comments?per_page=100"

/** The account AgentK posts as. Named so a reader can filter a thread by it
 *  themselves rather than taking our word for which comments are the agent's. */
export const AGENTK_BOT_LOGIN = "yb-agentk-dev[bot]";

/** The day these threads were read off GitHub. Rendered next to them. */
export const PUBLIC_REVIEWS_READ_ON = "24 September 2026";

export const YUGABYTE_DB_REPO = "yugabyte/yugabyte-db";

/** Where the deployment that posted these lives. Internal to Yugabyte, so it
 *  is offered as a pointer rather than as something the reader can open. */
export const AGENTK_DASHBOARD_URL = "https://agentk.dev.yugabyte.com:3000/code-review";

export type PublicRun = {
  pr: number;
  title: string;
  /** The engineer who opened it, not the reviewer. */
  author: string;
  state: "open" | "merged";
  /** AgentK's own comments on that pull request, by severity. A review that
   *  found nothing is a real outcome and renders as one. */
  major: number;
  minor: number;
};

export function pullRequestUrl(pr: number): string {
  return `https://github.com/${YUGABYTE_DB_REPO}/pull/${pr}`;
}

/** A spread of real reviews: two security-shaped ones with a lot to say, a
 *  couple of ordinary ones, and one the agent had nothing to flag on. The
 *  clean row is deliberate. A list where every review finds something reads
 *  as a demo. */
export const PUBLIC_RUNS: PublicRun[] = [
  {
    pr: 34141,
    title: "YCQL: Support HASHED PASSWORD on CREATE ROLE and ALTER ROLE",
    author: "ajcaldera1",
    state: "open",
    major: 5,
    minor: 2,
  },
  {
    pr: 34089,
    title: "YCQL: Redact passwords in audit records for rejected statements",
    author: "ajcaldera1",
    state: "open",
    major: 2,
    minor: 3,
  },
  {
    pr: 34178,
    title: "YSQL: Batch catalog preload requests by byte limits instead of row count limits",
    author: "iSignal",
    state: "open",
    major: 1,
    minor: 2,
  },
  {
    pr: 34076,
    title: "DocDB: Judge the write fence against the op's assigned hybrid time",
    author: "mdbridge",
    state: "open",
    major: 1,
    minor: 0,
  },
  {
    pr: 34014,
    title: "DocDB: Export SST statistics as Prometheus gauges",
    author: "balajisubramanian-yugabyte",
    state: "open",
    major: 0,
    minor: 5,
  },
  {
    pr: 34153,
    title: "DocDB: Keep the colocated tombstone cache across schema GC and backfill done",
    author: "hkimura",
    state: "open",
    major: 0,
    minor: 1,
  },
  {
    pr: 34152,
    title: "DocDB: Support UINT32 columns in yb_thin_client so it can open tables keyed by oids",
    author: "yamen-haddad",
    state: "merged",
    major: 0,
    minor: 0,
  },
];

export type PublicReviewComment = {
  /** GitHub's own comment id, which is also its anchor. */
  id: number;
  severity: "major" | "minor";
  path: string;
  line: number | null;
  /** AgentK's comment, trimmed to the claim and the reasoning under it. The
   *  real ones run several times this long, which is what the link is for. */
  body: string;
  /** What the engineer who received it wrote back, trimmed the same way. */
  reply: string;
  replyAuthor: string;
};

/** The thread Chapter 4 reads in full. Chosen because the loop closes in
 *  public: the agent makes a specific, checkable claim, and the author
 *  agrees and fixes it. A review nobody answered would not show that. */
export const FEATURED_REVIEW = {
  pr: 34141,
  title: "YCQL: Support HASHED PASSWORD on CREATE ROLE and ALTER ROLE",
  author: "ajcaldera1",
  comments: [
    {
      id: 4093938320,
      severity: "major",
      path: "src/yb/yql/cql/ql/audit/audit_logger.cc",
      line: null,
      body: "**The claim:** lines 495-496 say a dollar-quoted string is a \"non-string construct\" whose \"body can't be a password value\". That is false.\n\n**Why it fails:**\n- In `scanner_lex.l:679-698`, `$tag$…$tag$` returns `SCONST`.\n- `RoleOption: PASSWORD '=' Sconst` accepts it.\n- So `CREATE ROLE r WITH PASSWORD = $$secret$$` is valid.\n- This branch copies `$$secret$$` into the audit record verbatim, on both the success path and the failure path.",
      reply:
        "Right, the comment was wrong. Fixed in #34089: the password branch now takes any string constant after `=`, which covers `'...'`, `E'...'`, `U&'...'`, and `$tag$...$tag$`. I also corrected the loop comment.",
      replyAuthor: "ajcaldera1",
    },
    {
      id: 4093938307,
      severity: "major",
      path: "src/yb/yql/cql/ql/audit/audit_logger.cc",
      line: null,
      body: "New since the last round: the token-aware scanner that replaced quote parity treats any `$` as a possible dollar-quote opener, including a `$` in the middle of an unquoted identifier. The CQL lexer allows `$` inside identifiers, so `CREATE ROLE a$$ WITH PASSWORD = 'secret'` is a valid statement.\n\nThe scanner builds the delimiter `$$`, finds no closing `$$`, and copies the rest of the statement verbatim. The password is then audited in cleartext.",
      reply:
        "Fixed in #34089 as suggested. A `$` or `E'` now opens a token only when the previous character isn't `ident_cont`, and the tag has to follow `dolq_start`/`dolq_cont`, so `$1$` is not a delimiter. Added a `TestAudit.auth` case.",
      replyAuthor: "ajcaldera1",
    },
    {
      id: 4084298516,
      severity: "minor",
      path: "src/yb/yql/cql/ql/ptree/pt_create_role.cc",
      line: 82,
      body: "This validator is meant to accept only hashes that `bcrypt_checkpw` can verify, but some accepted hashes can never verify. Position 28 (the last salt character) and position 59 (the last checksum character) each carry fewer than 6 significant bits, and any base64 character is accepted there.\n\nThat gives the same \"role exists but can never authenticate\" result that the cost range check was added to prevent.",
      reply:
        "Good catch -- fixed in `IsValidBcryptHash`. Rather than whitelisting the canonical characters, I validate the padding bits directly: the last salt character must have its low 4 bits zero, and the last checksum character its low 2 bits zero.",
      replyAuthor: "ajcaldera1",
    },
  ] satisfies PublicReviewComment[],
} as const;

export function reviewCommentUrl(pr: number, commentId: number): string {
  return `${pullRequestUrl(pr)}#discussion_r${commentId}`;
}
