// node --test .github/scripts/pr-title.test.js
const test = require("node:test");
const assert = require("node:assert/strict");
const { parseTitle, run } = require("./pr-title.js");

test("accepts one or more tags back to back", () => {
  for (const [title, tags] of [
    ["[AMP-55] compute: ship unaccent", ["AMP-55"]],
    ["[#512] api_server: serve over PAT", ["#512"]],
    ["[CLOUDGA-37256] api_server: fix the hint", ["CLOUDGA-37256"]],
    ["[AMP-55][#512] compute: one write path", ["AMP-55", "#512"]],
    ["[MEKO-1][AMP-2][#3] ci: x", ["MEKO-1", "AMP-2", "#3"]],
    ["[A2B-7] docs: digits after the first letter", ["A2B-7"]],
  ]) {
    assert.deepEqual(parseTitle(title), { ok: true, tags }, title);
  }
});

test("rejects titles without a valid leading tag", () => {
  for (const title of [
    "compute: no tag",
    "",
    undefined,
    "[MEKO-xxx] api_server: placeholder",
    "[AMP-0] compute: zero",
    "[#0] compute: zero",
    "[#012] compute: leading zero",
    "[amp-55] compute: lower case",
    "[Amp-55] compute: mixed case",
    "[A-55] compute: one-letter project",
    "[AMP-55, #512] compute: comma list",
    "AMP-55: compute: no brackets",
    "#512 compute: no brackets",
    "(#512) compute: parentheses",
    " [AMP-55] compute: leading space",
    "compute: tag at the end [AMP-55]",
    "Fix [AMP-55] in compute",
  ]) {
    assert.equal(parseTitle(title).ok, false, String(title));
  }
});

test("rejects tags separated by a space", () => {
  const r = parseTitle("[AMP-55] [#512] compute: spaced");
  assert.equal(r.ok, false);
  assert.match(r.reason, /back to back/);
});

test("requires one space and a title after the tags", () => {
  for (const title of ["[AMP-55]", "[AMP-55] ", "[AMP-55]compute: x", "[AMP-55]  compute: two spaces"]) {
    const r = parseTitle(title);
    assert.equal(r.ok, false, title);
    assert.match(r.reason, /one space and the title/, title);
  }
});

test("names a bracket that is not a tag, after valid tags too", () => {
  for (const [title, bad] of [
    ["[MEKO-xxx] api_server: x", "[MEKO-xxx]"],
    ["[AMP-55][MEKO-xxx] api_server: x", "[MEKO-xxx]"],
    ["[AMP-55] [MEKO-xxx] api_server: x", "[MEKO-xxx]"],
    ["[AMP-55, #512] compute: x", "[AMP-55, #512]"],
  ]) {
    const r = parseTitle(title);
    assert.equal(r.ok, false, title);
    assert.equal(r.reason, `\`${bad}\` is not a ticket tag`, title);
  }
  assert.match(parseTitle("compute: x").reason, /does not start with a ticket tag/);
  assert.match(parseTitle(" [AMP-55] compute: x").reason, /does not start with a ticket tag/);
});

function fakeCore() {
  const core = { failed: null, infos: [] };
  core.info = m => core.infos.push(m);
  core.setFailed = m => { core.failed = m; };
  return core;
}

function ctx(title, type = "User", login = "someone") {
  return { payload: { pull_request: { title, user: { type, login } } } };
}

test("run passes a tagged title", async () => {
  const core = fakeCore();
  await run({ context: ctx("[AMP-55][#512] compute: x"), core });
  assert.equal(core.failed, null);
  assert.deepEqual(core.infos, ["Tags: AMP-55, #512"]);
});

test("run fails an untagged title with the accepted forms", async () => {
  const core = fakeCore();
  await run({ context: ctx("compute: x"), core });
  assert.match(core.failed, /^PR title "compute: x": it does not start with a ticket tag\./);
  assert.match(core.failed, /\[AMP-55\]\[#512\] compute/);
});

test("run exempts bots", async () => {
  const core = fakeCore();
  await run({ context: ctx("ci: bump the github-actions group", "Bot", "dependabot[bot]"), core });
  assert.equal(core.failed, null);
  assert.match(core.infos[0], /dependabot\[bot\], a bot: exempt/);
});
