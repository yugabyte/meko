#!/usr/bin/env node
// npm run check:demo-review [-- --sock <web.sock>] [--runs 3]
//
// Measures that the AgentK tour's Level 1 trap needs Meko (docs/design/workshops.md): against a
// running AgentK with a real coding agent and Meko, it runs the code review of the teammate's fix
// (demo/score-fix) <runs> times with the demo's MEKO.md, which lists the gotchas datapack, and
// <runs> times with MEKO.md taken out of the worktree, so the review has no datapacks. It passes only
// when every review with the datapack keeps a finding that credits MG-2, and no review without it
// keeps a finding about the trap. Run it by hand whenever the patch or gotchas.md changes, and put
// its result in the PR: it isn't in CI, since every run is a real review that costs tokens.
//
// It talks to the server on its unix socket (`npm run dev`'s ./.dev-home/web.sock by default), opens
// the tour's project (starting the tour if it isn't open), and archives every session it starts.
import { request } from 'node:http';
import { existsSync, rmSync } from 'node:fs';
import { parseArgs } from 'node:util';

const TOUR = 'agentk-101';
const BRANCH = 'demo/score-fix';
const TRAP = 'MG-2';
/** A finding about the trap without a credit: it says the reset breaks a score carried across rounds or games. */
const ABOUT_TRAP = /newGame[^]*(round|challenge|carr(y|ies|ied)|total|across|between)|(round|challenge|carr(y|ies|ied)|total|across|between)[^]*newGame/i;
const POLL_MS = 5000;
const REVIEW_TIMEOUT_MS = 45 * 60_000;

const { values } = parseArgs({ options: { sock: { type: 'string', default: '.dev-home/web.sock' }, runs: { type: 'string', default: '3' } } });
const sock = values.sock;
const runs = Number(values.runs);
if (!existsSync(sock)) {
  console.error(`No AgentK server socket at ${sock}. Start one (npm run dev), or name it with --sock.`);
  process.exit(2);
}

function api(method, path, body) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? undefined : JSON.stringify(body);
    const req = request({ socketPath: sock, path, method, headers: { host: 'localhost', ...(data ? { 'content-type': 'application/json' } : {}) } }, (res) => {
      let text = '';
      res.on('data', (c) => (text += c));
      res.on('end', () => {
        const json = text ? JSON.parse(text) : null;
        if (res.statusCode >= 400) reject(new Error(`${method} ${path}: ${res.statusCode} ${json?.error ?? text}`));
        else resolve(json);
      });
    });
    req.on('error', reject);
    if (data) req.write(data);
    req.end();
  });
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const session = async (id) => (await api('GET', '/api/state')).projects.flatMap((p) => p.sessions).find((s) => s.id === id);

async function until(what, fn, ms) {
  const end = Date.now() + ms;
  for (;;) {
    const v = await fn();
    if (v) return v;
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await sleep(POLL_MS);
  }
}

/** One review of the teammate's fix; `withMeko` false takes MEKO.md out of the worktree first. */
async function review(projectId, n, withMeko) {
  const name = `check-demo-review ${withMeko ? 'with' : 'without'} Meko ${n}`;
  const s = await api('POST', `/api/projects/${projectId}/sessions`, {
    name, start: 'code', branch: BRANCH, existingBranch: true,
    prompt: 'Read the change on this branch against main and say in one line what it does. Change nothing: no edits, no commits.',
  });
  try {
    await until(`${name}'s first turn`, async () => (await session(s.id))?.status === 'idle', 10 * 60_000);
    if (!withMeko) rmSync(`${s.worktreePath}/MEKO.md`, { force: true });
    await api('POST', `/api/sessions/${s.id}/actions/review-run`, {});
    const detail = await until(`${name}'s code review`, async () => {
      const d = await api('GET', `/api/sessions/${s.id}/code-review`).catch(() => null);
      // The tour's findings wait on the user (triage) once a round is done: that's as far as this needs.
      return d && ['done', 'triage', 'failed', 'cancelled'].includes(d.review.state) ? d : null;
    }, REVIEW_TIMEOUT_MS);
    if (detail.review.state !== 'done' && detail.review.state !== 'triage') throw new Error(`${name}'s code review ${detail.review.state}: ${detail.review.detail ?? ''}`);
    const kept = detail.findings.filter((f) => f.kept);
    const credited = kept.some((f) => f.gotchas.includes(TRAP));
    const mentioned = kept.some((f) => ABOUT_TRAP.test(f.comment));
    return { name, withMeko, credited, caught: withMeko ? credited : mentioned, datapacks: detail.datapacks.map((d) => d.name), kept };
  } finally {
    await api('POST', `/api/sessions/${s.id}/actions/archive`, {}).catch((e) => console.warn(`couldn't archive ${name}: ${e.message}`));
  }
}

const tour = (await api('GET', '/api/workshops')).find((w) => w.id === TOUR);
if (!tour) throw new Error(`This AgentK has no ${TOUR} workshop.`);
const projectId = tour.projectId ?? (await api('POST', `/api/workshops/${TOUR}/start`, {})).projectId;

// The gotchas must be indexed before the measured reviews, which this script runs by hand. The
// level's open session in the tour, or else a new one, gets AgentK's review by itself: a first look
// while the gotchas datapack is empty, then (on the user's go, given here) the gotchas and the
// review proper; once a finished tour put the gotchas in, the review proper alone.
const open = (await api('GET', '/api/state')).projects.find((p) => p.id === projectId)?.sessions.find((s) => s.branch === BRANCH && s.codeReview);
const level = open ?? await api('POST', `/api/projects/${projectId}/sessions`, {
  name: 'check-demo-review gotchas', start: 'code', branch: BRANCH, existingBranch: true, prompt: 'Reply in one line that you are waiting, then stop. Change nothing.',
});
const settled = (d) => ['done', 'triage', 'failed', 'cancelled'].includes(d.review.state);
const first = await until("AgentK's review of the level", async () => {
  const d = await api('GET', `/api/sessions/${level.id}/code-review`).catch(() => null);
  return d && settled(d) ? d : null;
}, REVIEW_TIMEOUT_MS);
if (first.review.firstLook && first.review.state !== 'done') throw new Error(`AgentK's first look ${first.review.state}: ${first.review.detail ?? ''}`);
if (first.review.firstLook) {
  // Nothing waits for a go (409) when the tour's user already gave it.
  await api('POST', `/api/workshops/${TOUR}/go`, {}).catch(() => undefined);
  await until("AgentK's review with the gotchas", async () => {
    const d = await api('GET', `/api/sessions/${level.id}/code-review`).catch(() => null);
    return d && !d.review.firstLook && settled(d) ? d : null;
  }, REVIEW_TIMEOUT_MS * 2);
}
if (!open) await api('POST', `/api/sessions/${level.id}/actions/archive`, {}).catch(() => undefined);

const results = [];
for (let i = 1; i <= runs; i++) {
  // One with and one without at a time.
  results.push(...await Promise.all([review(projectId, i, true), review(projectId, i, false)]));
}

let ok = true;
for (const r of results) {
  const good = r.withMeko ? r.caught : !r.caught;
  ok &&= good;
  console.log(`${good ? 'ok  ' : 'FAIL'} ${r.name}: ${r.caught ? 'caught the trap' : 'missed the trap'}${r.withMeko ? (r.credited ? ` (credits ${TRAP})` : '') : ''}; datapacks: ${r.datapacks.join(', ') || 'none'}`);
  for (const f of r.kept) console.log(`       ${f.severity} ${f.file}:${f.line}${f.gotchas.length ? ` [${f.gotchas.join(', ')}]` : ''} ${f.comment.split('\n')[0].slice(0, 160)}`);
}
console.log(ok
  ? `Measured: the trap was caught in all ${runs} reviews with the gotchas datapack and in none of the ${runs} without.`
  : 'The trap doesn\'t need Meko yet: change the patch or gotchas.md until every review with the datapack catches it and none without does.');
process.exit(ok ? 0 : 1);
