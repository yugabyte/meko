import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { FastifyInstance } from 'fastify';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import WebSocket from 'ws';
import { emptyBrief, type Deck } from '../../src/shared/deck.js';
import type { RunState } from '../../src/shared/run.js';
import type { ChangeSet, DeckState, DeckSummary, Health, Job, RunServerMessage } from '../../src/shared/types.js';
import { runClaude } from '../../src/server/claude.js';
import { DeckStore } from '../../src/server/decks.js';
import { Jobs } from '../../src/server/jobs.js';
import { getPaths } from '../../src/server/paths.js';
import { LIVE_AFTER_MOVE_MS, Runs } from '../../src/server/runs.js';
import { broadcast, buildServer, type Core } from '../../src/server/server.js';
import { FAKE_CLAUDE, FakeMeko, fakeAnswers, tempDir } from '../helpers.js';

const SVG = Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"/>').toString('base64');

let dir: string;
let meko: FakeMeko;
let core: Core;
let app: FastifyInstance;
let mekoChanges = 0;

async function makeCore(home: string, webDir: string | null = null): Promise<{ core: Core; app: FastifyInstance }> {
  const paths = getPaths(home);
  const events = new Set<WebSocket>();
  const decks = new DeckStore(paths, meko, (state) => broadcast({ events }, { type: 'deck', state }));
  const jobs = new Jobs({
    decks, meko, claude: [process.execPath, FAKE_CLAUDE], run: runClaude,
    mekoCreds: async () => ({ url: 'http://127.0.0.1:7778/mcp', key: 'k-secret' }),
    emit: (e) => broadcast({ events }, e),
  });
  const c: Core = {
    version: '1.0.0', meko, decks, jobs, runs: new Runs(decks, join(paths.run, 'runs')), events, claudeFound: () => true,
    mekoChanged: () => void mekoChanges++,
  };
  return { core: c, app: await buildServer(c, { webDir }) };
}

beforeEach(async () => {
  dir = tempDir('pres-srv-');
  meko = new FakeMeko();
  await meko.createDatapack('ybdb/xcluster', 'xCluster notes');
  process.env.FAKE_CLAUDE_DIR = dir;
  ({ core, app } = await makeCore(join(dir, 'home')));
});
afterEach(async () => {
  await app.close();
  rmSync(dir, { recursive: true, force: true });
});

const api = async <T>(method: 'GET' | 'POST' | 'PUT', url: string, payload?: unknown, headers: Record<string, string> = {}) => {
  const r = await app.inject({ method, url, payload: payload as never, headers: { host: 'meko.test:7777', ...headers } });
  return { status: r.statusCode, body: r.json() as T, headers: r.headers };
};

async function createDeck(): Promise<DeckState> {
  const r = await api<DeckState>('POST', '/api/decks', { title: 'YugabyteDB for Acme', brief: { ...emptyBrief(), audience: 'Acme' }, sources: { content: ['ybdb/xcluster'] } });
  expect(r.status).toBe(200);
  return r.body;
}

async function settled(deckId: string, jobId: string): Promise<Job> {
  for (let i = 0; i < 200; i++) {
    const j = core.jobs.list(deckId).find((x) => x.id === jobId)!;
    if (j.status !== 'queued' && j.status !== 'running') return j;
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error('the job never ended');
}

const calls = () => readFileSync(join(dir, 'calls.jsonl'), 'utf8').trim().split('\n').map((l) => JSON.parse(l) as { args: string[]; input: string; key: string | null });

const mapAnswer = {
  summary: 'A product menu after the opening.',
  ops: [
    { op: 'addScene', scene: { id: 'ybdb', title: 'YugabyteDB', intent: 'Distributed SQL', slides: [], x: 260, y: 0 } },
    { op: 'connect', edge: { id: 'e-open-ybdb', from: 'opening', to: 'ybdb', kind: 'next' } },
  ],
};
const buildAnswer = {
  summary: 'Two slides on YugabyteDB.',
  ops: [
    { op: 'addSlide', scene: 'ybdb', slide: { id: 's-ybdb-1', layout: 'diagram-right', variants: { overview: { blocks: [{ type: 'heading', text: 'Postgres, distributed' }, { type: 'image', asset: 'topology.svg', alt: 'Three regions' }] } }, notes: '', sources: [{ kind: 'memory', datapack: 'ybdb/xcluster', id: 'm1' }] } },
  ],
  files: [{ name: 'topology.svg', path: 'assets/topology.svg', type: 'image/svg+xml' }],
};

describe('building a deck with Claude', () => {
  it('drafts the map, builds a scene with a file Claude made, and returns an ask as a change set that spares hand edits', async () => {
    const deck = await createDeck();
    expect(deck.name).toBe('presenter/deck/yugabytedb-for-acme');
    const opening = deck.deck.scenes.opening!.slides[0]!;
    fakeAnswers(dir, [
      { match: 'Draft the story map', answer: mapAnswer },
      { match: 'Build the slides of scene "YugabyteDB"', answer: buildAnswer, files: { 'assets/topology.svg': SVG } },
      { match: 'The author asks', answer: { summary: 'Shorter.', ops: [{ op: 'updateSlide', slide: { id: opening, layout: 'title', variants: { overview: { blocks: [{ type: 'heading', text: 'Short' }] } }, notes: '', sources: [] } }] } },
    ]);

    const map = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' });
    expect((await settled(deck.id, map.body.id)).status).toBe('done');
    const build = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'build', scenes: ['ybdb'] });
    const built = await settled(deck.id, build.body.id);
    expect(built.status, built.error ?? '').toBe('done');
    expect(built.activity).toContain('YugabyteDB: Meko: context search "xcluster lag"');
    let d = (await api<DeckState>('GET', `/api/decks/${deck.id}`)).body.deck;
    expect(d.scenes.ybdb!.slides).toEqual(['s-ybdb-1']);
    const svg = await app.inject({ method: 'GET', url: `/api/decks/${deck.id}/assets/topology.svg`, headers: { host: 'meko.test:7777' } });
    expect([svg.statusCode, svg.headers['content-type'], svg.headers['content-security-policy']]).toEqual([200, 'image/svg+xml', "default-src 'none'; style-src 'unsafe-inline'; sandbox"]);

    // The author edits the opening slide by hand, then asks about it.
    const hand = { ...d.slides[opening]!, notes: 'mine', handEdited: true };
    expect((await api('POST', `/api/decks/${deck.id}/ops`, { ops: [{ op: 'updateSlide', slide: hand }] })).status).toBe(200);
    const ask = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'ask', instruction: 'Shorter', scope: { kind: 'slide', slide: opening } });
    expect((await settled(deck.id, ask.body.id)).changeSet).toBeTruthy();
    const sets = (await api<ChangeSet[]>('GET', `/api/decks/${deck.id}/changesets`)).body;
    expect(sets).toHaveLength(1);
    expect((await api<DeckState>('GET', `/api/decks/${deck.id}`)).body.deck.slides[opening]!.notes).toBe('mine');
    expect((await api('POST', `/api/changesets/${sets[0]!.id}/apply`, { take: [0] })).status).toBe(200);
    d = (await api<DeckState>('GET', `/api/decks/${deck.id}`)).body.deck;
    expect(d.slides[opening]!.variants.overview!.blocks).toEqual([{ type: 'heading', text: 'Short' }]);

    // One session for the deck: started by the first job, resumed by the ask, forked by the build.
    const [first, second, third] = calls();
    const session = first!.args[first!.args.indexOf('--session-id') + 1];
    expect(second!.args.join(' ')).toContain(`--resume ${session} --fork-session`);
    expect(third!.args.join(' ')).toContain(`--resume ${session}`);
    expect(third!.args).not.toContain('--fork-session');
    expect(calls().every((c) => c.key === 'k-secret' && !c.args.join(' ').includes('k-secret'))).toBe(true);
  });

  it.each([
    ['after a map, on a fork of the deck\'s session', true],
    ['with no deck session yet, on a session of the scene\'s own', false],
  ])('asks a scene again on its own session %s, and leaves the deck\'s session alone', async (_n, mapFirst) => {
    const deck = await createDeck();
    const slideWith = (block: unknown) => ({ id: 's-y', layout: 'bullets', variants: { overview: { blocks: [block] } }, notes: '', sources: [] });
    fakeAnswers(dir, [
      { match: 'Draft the story map', answer: mapAnswer },
      { match: 'Build the slides of scene', answer: { summary: 'x', ops: [{ op: 'addSlide', scene: 'opening', slide: slideWith({ type: 'image', asset: 'missing.png', alt: '' }) }] } },
      { match: 'couldn\'t use that answer', answer: { summary: 'ok', ops: [{ op: 'addSlide', scene: 'opening', slide: slideWith({ type: 'heading', text: 'Y' }) }] } },
    ]);
    if (mapFirst) await settled(deck.id, (await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' })).body.id);
    const sessionFile = join(dir, 'home', 'decks', deck.id, 'session.json');
    const afterMap = mapFirst ? JSON.parse(readFileSync(sessionFile, 'utf8')) as { id: string; started: boolean } : null;
    const build = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'build', scenes: ['opening'] });
    expect((await settled(deck.id, build.body.id)).status).toBe('done');
    const [first, retry] = calls().slice(mapFirst ? 1 : 0).map((c) => c.args);
    // The retry continues the scene's run, which Claude kept: never --no-session-persistence.
    const firstId = first!.includes('--fork-session') ? null : first![first!.indexOf('--session-id') + 1]!;
    expect(first).not.toContain('--no-session-persistence');
    expect(retry![retry!.indexOf('--resume') + 1]).toMatch(firstId ?? /^fork-/);
    expect(retry).not.toContain('--fork-session');
    const after = JSON.parse(readFileSync(sessionFile, 'utf8')) as { id: string; started: boolean };
    if (afterMap) expect(after).toEqual(afterMap);
    else expect([after.started, after.id === firstId]).toEqual([false, false]);
  });

  it('asks again when Claude adds an asset itself, since only the files it lists have their bytes here', async () => {
    const deck = await createDeck();
    const asset = { op: 'addAsset', name: 'chart.svg', asset: { hash: 'a'.repeat(64), size: 10, type: 'image/svg+xml' } };
    fakeAnswers(dir, [{ match: 'story map', answer: { summary: 'x', ops: [asset, ...mapAnswer.ops] } }, { match: 'couldn\'t use that answer', answer: mapAnswer }]);
    const job = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' });
    expect((await settled(deck.id, job.body.id)).status).toBe('done');
    expect(calls()[1]!.input).toContain('Don\'t use addAsset');
    expect((await api<DeckState>('GET', `/api/decks/${deck.id}`)).body.deck.assets).toEqual({});
  });

  it('asks again once when the answer would break the deck, then says why', async () => {
    const deck = await createDeck();
    const bad = { summary: 'x', ops: [{ op: 'connect', edge: { id: 'e', from: 'opening', to: 'nowhere', kind: 'next' } }] };
    fakeAnswers(dir, [{ match: 'story map', answer: bad }, { match: 'couldn\'t use that answer', answer: bad }]);
    const job = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' });
    const ended = await settled(deck.id, job.body.id);
    expect(ended.status).toBe('failed');
    expect(ended.error).toMatch(/would break the deck: .*doesn't have/);
    expect(calls()).toHaveLength(2);
    expect(calls()[1]!.input).toMatch(/^Presenter couldn't use that answer/);
  });
});

describe('saving and history', () => {
  it('saves to the deck\'s datapack, opens there from a fresh home, and restores an older version', async () => {
    const deck = await createDeck();
    expect((await api<DeckState>('POST', `/api/decks/${deck.id}/save`)).body.dirty).toBe(false);
    await api('POST', `/api/decks/${deck.id}/ops`, { ops: [{ op: 'setTitle', title: 'Acme, take two' }] });
    const saved = (await api<DeckState>('POST', `/api/decks/${deck.id}/save`)).body;
    const versions = (await api<{ hash: string }[]>('GET', `/api/decks/${deck.id}/versions`)).body;
    expect(versions).toHaveLength(2);

    await app.close();
    ({ core, app } = await makeCore(join(dir, 'other-home')));
    expect((await api<DeckState>('GET', `/api/decks/${deck.id}`)).body.deck).toEqual(saved.deck);
    const restored = (await api<DeckState>('POST', `/api/decks/${deck.id}/restore`, { hash: versions[1]!.hash })).body;
    expect([restored.deck.title, restored.dirty, restored.canUndo]).toEqual(['YugabyteDB for Acme', true, true]);
  });

  it('sends an asset in ranges, and not again while the browser has it', async () => {
    const deck = await createDeck();
    const put = await app.inject({ method: 'PUT', url: `/api/decks/${deck.id}/assets/clip.mp4`, payload: Buffer.from('0123456789'), headers: { host: 'meko.test:7777', 'content-type': 'application/octet-stream', 'x-asset-type': 'video/mp4' } });
    expect(put.statusCode).toBe(200);
    const get = (headers: Record<string, string> = {}) => app.inject({ method: 'GET', url: `/api/decks/${deck.id}/assets/clip.mp4`, headers: { host: 'meko.test:7777', ...headers } });
    const whole = await get();
    expect([whole.statusCode, whole.body, whole.headers['accept-ranges'], whole.headers['cache-control']]).toEqual([200, '0123456789', 'bytes', 'private, no-cache']);
    const part = await get({ range: 'bytes=2-4' });
    expect([part.statusCode, part.body, part.headers['content-range']]).toEqual([206, '234', 'bytes 2-4/10']);
    expect((await get({ range: 'bytes=-3' })).body).toBe('789');
    expect((await get({ range: 'bytes=10-' })).statusCode).toBe(416);
    expect((await get({ 'if-none-match': String(whole.headers.etag) })).statusCode).toBe(304);
  });

  it('refuses an asset over 100 MiB when it\'s added', async () => {
    const deck = await createDeck();
    const r = await app.inject({ method: 'PUT', url: `/api/decks/${deck.id}/assets/big.mp4`, payload: Buffer.alloc(100 * 1024 * 1024 + 1), headers: { host: 'meko.test:7777', 'content-type': 'application/octet-stream' } });
    expect([r.statusCode, r.json().error]).toEqual([413, 'The file is bigger than 100 MiB, the most one asset can be. Trim or compress it first.']);
  });
});

describe('behind Meko\'s proxy', () => {
  it('refuses another site\'s writes, and serves its page under /ext/presenter/ unframed', async () => {
    const deck = await createDeck();
    const r = await api<{ error: string }>('POST', `/api/decks/${deck.id}/save`, {}, { origin: 'https://evil.test' });
    expect([r.status, r.body.error]).toEqual([403, 'Cross-origin request refused.']);
    expect((await api('POST', `/api/decks/${deck.id}/save`, {}, { origin: 'https://meko.test:7777' })).status).toBe(200);

    const web = join(dir, 'web');
    mkdirSync(join(web, 'assets'), { recursive: true });
    writeFileSync(join(web, 'index.html'), '<html><head></head><body></body></html>');
    await app.close();
    ({ core, app } = await makeCore(join(dir, 'home'), web));
    const page = await app.inject({ method: 'GET', url: '/decks/x/map', headers: { host: 'meko.test:7777' } });
    expect(page.body).toContain('<base href="/ext/presenter/" />');
    expect(page.headers['x-frame-options']).toBe('DENY');
    expect(page.headers['content-security-policy']).toContain("frame-ancestors 'none'");
  });
});

describe('when Meko\'s Meko Cloud link changes', () => {
  it('hears Meko\'s notice from Meko alone', async () => {
    const notice = (what: string, headers: Record<string, string> = {}) => app.inject({ method: 'POST', url: '/_meko/changed', payload: { what }, headers: { host: 'meko.test:7777', ...headers } });
    expect((await notice('cloud')).statusCode).toBe(204);
    expect((await notice('cloud', { origin: 'https://evil.test' })).statusCode).toBe(403);
    expect((await notice('something-else')).statusCode).toBe(204);
    expect(mekoChanges).toBe(1);
  });

  it('says where decks are saved, and keeps a draft of the Meko before from being edited, but not from being played', async () => {
    expect((await api<Health>('GET', '/api/health')).body.mekoAt).toEqual({ kind: 'local', host: '127.0.0.1:7778' });
    const deck = await createDeck();
    const run = (await api<RunState>('POST', `/api/decks/${deck.id}/runs`, {})).body;

    // Linked to Meko Cloud, which hasn't the local Meko's datapacks.
    meko.where = { kind: 'cloud', host: 'mcp.meko.example' };
    const local = meko.datapacks;
    meko.datapacks = [];
    expect((await api<Health>('GET', '/api/health')).body.mekoAt).toEqual({ kind: 'cloud', host: 'mcp.meko.example' });
    expect((await api<DeckSummary[]>('GET', '/api/decks')).body).toEqual([expect.objectContaining({ id: deck.id, draft: true, elsewhere: { kind: 'local', host: '127.0.0.1:7778' } })]);
    const refused = "This deck is saved in the Meko on this computer, and Presenter now uses Meko Cloud (mcp.meko.example). Unlink Meko Cloud in Meko's Settings › Meko Cloud to open it.";
    for (const [method, url, body] of [
      ['GET', `/api/decks/${deck.id}`],
      ['POST', `/api/decks/${deck.id}/ops`, { ops: [{ op: 'setTitle', title: 'Moved' }] }],
      ['POST', `/api/decks/${deck.id}/save`],
      ['POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' }],
    ] as const) {
      const r = await api<{ error: string }>(method, url, body);
      expect([url, r.status, r.body.error]).toEqual([url, 409, refused]);
    }
    expect((await api('GET', `/api/runs/${run.id}`)).status).toBe(200);

    // A job queued before the link changed doesn't run Claude on the other Meko.
    const realList = meko.listDatapacks.bind(meko);
    meko.where = { kind: 'local', host: '127.0.0.1:7778' };
    meko.datapacks = local;
    meko.listDatapacks = async () => {
      meko.where = { kind: 'cloud', host: 'mcp.meko.example' };
      return realList();
    };
    const job = (await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' })).body;
    const ended = await settled(deck.id, job.id);
    expect([ended.status, ended.error]).toEqual(['failed', refused]);
    meko.listDatapacks = realList;

    // Unlinked again: the deck is the store's to change once more.
    meko.where = { kind: 'local', host: '127.0.0.1:7778' };
    meko.datapacks = local;
    expect((await api<DeckState>('POST', `/api/decks/${deck.id}/save`)).status).toBe(200);
  });
});

describe('a live run', () => {
  it('keeps the console and the audience on the same slide, and resumes after a restart', async () => {
    const deck = await createDeck();
    await api('POST', `/api/decks/${deck.id}/ops`, { ops: [
      { op: 'addScene', scene: { id: 'menu', title: 'Menu', intent: '', slides: [] } },
      { op: 'connect', edge: { id: 'e1', from: 'opening', to: 'menu', kind: 'next' } },
      { op: 'addSlide', scene: 'menu', slide: { id: 's-menu', layout: 'bullets', variants: { overview: { blocks: [] } }, notes: '', sources: [] } },
    ] });
    const run = (await api<RunState>('POST', `/api/decks/${deck.id}/runs`, {})).body;
    const sock = join(dir, 's.sock');
    await app.listen({ path: sock });
    const open = (origin = 'http://meko.test:7777') => new Promise<{ ws: WebSocket; got: RunServerMessage[] }>((resolve, reject) => {
      const ws = new WebSocket(`ws+unix://${sock}:/ws/runs/${run.id}`, { headers: { host: 'meko.test:7777', origin } });
      const got: RunServerMessage[] = [];
      ws.on('message', (m) => got.push(JSON.parse(String(m)) as RunServerMessage));
      ws.on('open', () => resolve({ ws, got }));
      ws.on('unexpected-response', (_req, res) => reject(new Error(`refused ${res.statusCode}`)));
    });
    await expect(open('https://evil.test')).rejects.toThrow(/refused 403/);
    const consoleWin = await open();
    const audience = await open();
    consoleWin.ws.send(JSON.stringify({ type: 'nav', nav: { action: 'next' } }));
    const seen = async (c: { got: RunServerMessage[] }) => {
      for (let i = 0; i < 100 && !c.got.some((m) => m.type === 'run' && m.run.pos.scene === 'menu'); i++) await new Promise((r) => setTimeout(r, 20));
      return c.got.filter((m): m is { type: 'run'; run: RunState } => m.type === 'run').at(-1)!.run.pos.scene;
    };
    expect([await seen(consoleWin), await seen(audience)]).toEqual(['menu', 'menu']);
    expect(audience.got[0]!.type).toBe('deck');
    consoleWin.ws.close();
    audience.ws.close();

    const restarted = new Runs(core.decks, join(getPaths(join(dir, 'home')).run, 'runs'));
    const back = await restarted.get(run.id);
    expect([back.run.pos.scene, (back.deck as Deck).title]).toEqual(['menu', 'YugabyteDB for Acme']);
  });

  it('tells Meko a talk is live while a window is open, or for a while after it last moved', async () => {
    const busy = async () => (await api<{ busy: boolean }>('GET', '/_meko/busy')).body.busy;
    expect(await busy()).toBe(false);
    const deck = await createDeck();
    const run = (await api<RunState>('POST', `/api/decks/${deck.id}/runs`, {})).body;
    expect(await busy()).toBe(false);
    const leave = await core.runs.join(run.id, { send: () => undefined });
    expect(await busy()).toBe(true);
    leave();
    expect(await busy()).toBe(false);
    await core.runs.nav(run.id, { action: 'next' });
    expect(await busy()).toBe(true);
    expect(core.runs.live(Date.now() + LIVE_AFTER_MOVE_MS)).toBe(false);
  });

  it('shows the windows a map Claude lands during the talk', async () => {
    const deck = await createDeck();
    const run = (await api<RunState>('POST', `/api/decks/${deck.id}/runs`, {})).body;
    const got: RunServerMessage[] = [];
    await core.runs.join(run.id, { send: (m) => got.push(m) });
    fakeAnswers(dir, [{ match: 'Draft the story map', answer: mapAnswer }]);
    const job = await api<Job>('POST', `/api/decks/${deck.id}/jobs`, { kind: 'map' });
    expect((await settled(deck.id, job.body.id)).status).toBe('done');
    const decks = got.filter((m): m is { type: 'deck'; deck: Deck } => m.type === 'deck');
    expect(Object.keys(decks.at(-1)!.deck.scenes)).toEqual(['opening', 'ybdb']);
  });
});
