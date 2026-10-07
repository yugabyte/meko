// Moves the Jira tickets a merged PR closes.
//
// A PR closes a ticket only with an explicit line in its body, the same shape
// GitHub uses for its own issues:
//
//   Closes AMP-55
//   Fixes https://yugabyte.atlassian.net/browse/MEKO-760, MEKO-761
//
// The line holds only the capitalized keyword and keys/URLs, so a wrapped prose
// line ("fixes the flake AMP-73 reported") or extra text ("Closes AMP-55 (found
// AMP-73)") closes nothing. A key in the title (`[AMP-55] ...`) or in prose is a
// reference, not a close: PRs routinely name tickets they make progress on or
// found (PR #1581 names AMP-73/74/75, which stay open).
//
// The merge comment this posts is the ticket's only link to the PR until the
// repo is added to the GitHub for Jira app, which links PRs from the key in the
// title or branch.

const URL = String.raw`https:\/\/yugabyte\.atlassian\.net\/browse\/[A-Z][A-Z0-9]+-\d+`;
const REF = String.raw`(?:\[[A-Z][A-Z0-9]+-\d+\]\(${URL}\)|(?:${URL})|[A-Z][A-Z0-9]+-\d+)`;
const CLOSE_LINE = new RegExp(String.raw`^\s*(?:[-*]\s+)?(?:Closes|Fixes|Resolves):?\s+(${REF}(?:\s*,\s*${REF}|\s+${REF})*)\s*\.?\s*$`);
const KEY = /\b([A-Z][A-Z0-9]+-\d+)\b/g;

// The projects this closes, each with its workflow's global Done transition
// (usable from any status).
const TARGETS = {
  AMP: { transition: "31", status: "Done" },
  MEKO: { transition: "31", status: "Done" },
};

function closedKeys(body, projects) {
  const keys = new Set();
  for (const line of (body || "").split(/\r?\n/)) {
    const m = line.match(CLOSE_LINE);
    if (!m) continue;
    for (const k of m[1].matchAll(KEY)) {
      if (projects.includes(k[1].split("-")[0])) keys.add(k[1]);
    }
  }
  return [...keys];
}

// The token is a service-account (scoped) API token: it is sent as a Bearer
// token to the api.atlassian.com gateway, not with basic auth to the site URL.
async function jira(path, init = {}) {
  const res = await fetch(`https://api.atlassian.com/ex/jira/${process.env.JIRA_CLOUD_ID}${path}`, {
    ...init,
    headers: {
      Authorization: `Bearer ${process.env.JIRA_API_TOKEN}`,
      Accept: "application/json",
      "Content-Type": "application/json",
    },
  });
  if (!res.ok) throw new Error(`${init.method || "GET"} ${path}: ${res.status} ${await res.text()}`);
  return res.status === 204 ? null : res.json();
}

function mergeComment(pr) {
  const text = t => ({ type: "text", text: t });
  return {
    type: "doc",
    version: 1,
    content: [{
      type: "paragraph",
      content: [
        text("Merged in "),
        { type: "text", text: pr.html_url, marks: [{ type: "link", attrs: { href: pr.html_url } }] },
        text(` (commit ${pr.merge_commit_sha.slice(0, 9)}): ${pr.title}`),
      ],
    }],
  };
}

async function run({ context, core }) {
  const pr = context.payload.pull_request;
  const keys = closedKeys(pr.body, Object.keys(TARGETS));
  if (!keys.length) {
    core.info("No `Closes <KEY>` line for an AMP or MEKO ticket; nothing to do.");
    return;
  }
  const dryRun = !process.env.JIRA_API_TOKEN;
  if (dryRun) core.warning("JIRA_API_TOKEN is not set: dry run, Jira is not touched.");

  let failed = false;
  for (const key of keys) {
    const target = TARGETS[key.split("-")[0]];
    if (dryRun) {
      core.info(`${key}: would comment the PR link and move it to ${target.status}.`);
      continue;
    }
    try {
      const { status } = (await jira(`/rest/api/3/issue/${key}?fields=status`)).fields;
      if (status.statusCategory.key === "done") {
        core.info(`${key}: already ${status.name}; left alone.`);
        continue;
      }
      await jira(`/rest/api/3/issue/${key}/comment`, { method: "POST", body: JSON.stringify({ body: mergeComment(pr) }) });
      await jira(`/rest/api/3/issue/${key}/transitions`, { method: "POST", body: JSON.stringify({ transition: { id: target.transition } }) });
      core.info(`${key}: ${status.name} -> ${target.status}.`);
    } catch (e) {
      failed = true;
      core.error(`${key}: ${e.message}`);
    }
  }
  if (failed) core.setFailed("Some Jira tickets were not moved; see the errors above.");
}

module.exports = { run, closedKeys, TARGETS };
