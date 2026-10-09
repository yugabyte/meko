// Checks that a PR title starts with its tracking tags.
//
// The repo squash-merges with the PR title as the commit subject, so this keeps
// the subjects on main tagged (see pr-title.yml for what it can't catch). A
// title starts with one or more tags written back to back, then one space and
// the title:
//
//   [AMP-55] compute: ...
//   [#512] api_server: ...
//   [AMP-55][#512] compute: ...
//
// `[#<n>]` is a GitHub issue in this repo and `[<KEY>-<n>]` a Jira key with an
// all-caps project. Only the format is checked; nothing is looked up. PRs that
// bots (Dependabot) open are exempt, since they have no ticket to carry.

const TAG = String.raw`\[(#[1-9]\d*|[A-Z][A-Z0-9]+-[1-9]\d*)\]`;
const PREFIX = new RegExp(String.raw`^(?:${TAG})*`);
const IS_TAG = new RegExp(String.raw`^${TAG}$`);

const FORMS = [
  "A PR title must start with its tracking tags, then one space and the title:",
  "  [AMP-55] compute: ...        a Jira ticket (all-caps project)",
  "  [#512] api_server: ...       a GitHub issue in this repo",
  "  [AMP-55][#512] compute: ...  several tags, back to back",
  "No ticket? File one; never invent a placeholder like [MEKO-xxx].",
].join("\n");

function parseTitle(title) {
  const t = title || "";
  const prefix = t.match(PREFIX)[0];
  const tags = [...prefix.matchAll(new RegExp(TAG, "g"))].map((x) => x[1]);
  const rest = t.slice(prefix.length);
  // A bracket right after the tags (or at the start) is a tag attempt.
  const next = rest.match(/^(\s*)(\[[^\]]*\])/);
  if (next && (!next[1] || tags.length)) {
    if (!IS_TAG.test(next[2]))
      return { ok: false, tags, reason: `\`${next[2]}\` is not a ticket tag` };
    return {
      ok: false,
      tags,
      reason: "several tags must be written back to back, with no space between them",
    };
  }
  if (!tags.length) return { ok: false, tags, reason: "it does not start with a ticket tag" };
  if (!/^ \S/.test(rest)) {
    return { ok: false, tags, reason: "the tags must be followed by one space and the title" };
  }
  return { ok: true, tags };
}

async function run({ context, core }) {
  const pr = context.payload.pull_request;
  if (pr.user.type === "Bot") {
    core.info(`Opened by ${pr.user.login}, a bot: exempt.`);
    return;
  }
  const { ok, tags, reason } = parseTitle(pr.title);
  if (ok) {
    core.info(`Tags: ${tags.join(", ")}`);
    return;
  }
  core.setFailed(`PR title "${pr.title}": ${reason}.\n\n${FORMS}`);
}

module.exports = { run, parseTitle };
