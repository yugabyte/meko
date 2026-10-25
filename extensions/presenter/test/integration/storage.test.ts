import { randomBytes } from 'node:crypto';
import { rmSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { applyOps } from '../../src/shared/ops.js';
import { INDEX_BYTES, joinBytes, MANIFEST_FILE, PART_BYTES, POINTER_FORMAT, bytesJson, splitBytes } from '../../src/shared/manifest.js';
import { emptyBrief, type Deck } from '../../src/shared/deck.js';
import type { DeckState } from '../../src/shared/types.js';
import { DeckStore } from '../../src/server/decks.js';
import { MekoError } from '../../src/server/meko.js';
import { getPaths } from '../../src/server/paths.js';
import { PieceStore, sha256 } from '../../src/server/pieces.js';
import { FakeMeko, sampleDeck, slide, tempDir } from '../helpers.js';

const MiB = 1024 * 1024;
let dir: string;
let meko: FakeMeko;
beforeEach(() => {
  dir = tempDir('pres-store-');
  meko = new FakeMeko();
});
afterEach(() => rmSync(dir, { recursive: true, force: true }));

const store = (name = 'cache') => new PieceStore(meko, join(dir, name));

/** The sample deck with a recorded demo: a 12 MiB video, kept in the store's cache as adding it does. */
function withVideo(s: PieceStore): { deck: Deck; video: Uint8Array } {
  const video = new Uint8Array(randomBytes(12 * MiB));
  const hash = s.keep(video);
  const deck = applyOps(sampleDeck(), [
    { op: 'addAsset', name: 'demo.mp4', asset: { hash, size: video.length, type: 'video/mp4' } },
    { op: 'updateSlide', slide: slide('s-x-2', [{ type: 'video', asset: 'demo.mp4' }]) },
  ]);
  return { deck, video };
}

describe('splitting into parts', () => {
  it.each([0, 1, PART_BYTES, PART_BYTES + 1, 38 * MiB])('round-trips %i bytes', (n) => {
    const b = new Uint8Array(randomBytes(n));
    const parts = splitBytes(b);
    expect(parts.every((p) => p.length <= PART_BYTES)).toBe(true);
    expect(parts.length).toBe(Math.max(1, Math.ceil(n / PART_BYTES)));
    expect(sha256(joinBytes(parts))).toBe(sha256(b));
  });
});

describe('a deck in its datapack', () => {
  it('saves past Meko\'s 5 MiB limit and loads back on another computer', async () => {
    const s = store();
    const { deck, video } = withVideo(s);
    const saved = await s.save('dp1', deck, new Set(), {});
    expect(Math.max(...(meko.artifacts.get('dp1') ?? []).map((a) => a.bytes.length))).toBeLessThanOrEqual(PART_BYTES);
    expect(meko.puts.at(-1)!.filename).toBe(MANIFEST_FILE);
    expect(meko.puts.filter((p) => p.filename.startsWith('parts/'))).toHaveLength(3);

    const other = store('other-cache');
    const versions = await other.versions('dp1');
    expect(versions[0]!.hash).toBe(saved.manifestHash);
    const loaded = await other.load('dp1', versions[0]!.hash);
    expect(loaded.deck).toEqual(deck);
    const back = await other.piece('dp1', loaded.assetPieces[deck.assets['demo.mp4']!.hash]!);
    expect(sha256(back)).toBe(sha256(video));
  });

  it('uploads only the changed scene and a new manifest', async () => {
    const s = store();
    const { deck } = withVideo(s);
    const first = await s.save('dp1', deck, new Set(), {});
    meko.puts = [];
    const changed = applyOps(deck, [{ op: 'updateSlide', slide: slide('s-amp-1', [{ type: 'heading', text: 'AMP, scales to zero' }]) }]);
    await s.save('dp1', changed, new Set(first.known), first.assetPieces);
    expect(meko.puts.map((p) => p.filename)).toEqual(['scenes/amp.json', MANIFEST_FILE]);
  });

  it('leaves the last save the newest when a save stops partway, and the retry sends only what\'s missing', async () => {
    const s = store();
    const { deck } = withVideo(s);
    const first = await s.save('dp1', deck, new Set(), {});
    const changed = applyOps(deck, [
      { op: 'updateSlide', slide: slide('s-amp-1', [{ type: 'heading', text: 'changed' }]) },
      { op: 'updateSlide', slide: slide('s-meko-1', [{ type: 'heading', text: 'changed too' }]) },
    ]);
    // The deck store keeps one set of known hashes across saves, as here.
    const known = new Set(first.known);
    meko.failAfter = 1;
    await expect(s.save('dp1', changed, known, first.assetPieces)).rejects.toThrow(/dropped/);
    expect((await s.versions('dp1'))[0]!.hash).toBe(first.manifestHash);

    meko.failAfter = Infinity;
    meko.puts = [];
    const second = await s.save('dp1', changed, known, first.assetPieces);
    expect((await s.versions('dp1'))[0]!.hash).toBe(second.manifestHash);
    expect(meko.puts.map((p) => p.filename)).toEqual(['scenes/meko.json', MANIFEST_FILE]);
    expect((await store('fresh').load('dp1', second.manifestHash)).deck).toEqual(changed);
  });

  it('can save again after a save that stopped partway while other uploads were under way', async () => {
    // The first upload fails at once; the others are still going when it does.
    class Flaky extends FakeMeko {
      n = 0;
      override async putArtifact(datapackId: string, filename: string, bytes: Uint8Array, type: string): Promise<string> {
        const i = this.n++;
        await new Promise((r) => setTimeout(r, i === 0 ? 5 : 30));
        if (i === 0) throw new MekoError('the connection dropped');
        return super.putArtifact(datapackId, filename, bytes, type);
      }
    }
    const flaky = new Flaky();
    const decks = new DeckStore(getPaths(join(dir, 'home')), flaky);
    const seen: (DeckState['saving'])[] = [];
    decks.listen((s) => seen.push(s.saving));
    const d = await decks.create({ title: 'Partway', brief: emptyBrief(), sources: { content: [], template: null, animations: [] } });
    for (const n of ['a.png', 'b.png', 'c.png', 'd.png']) await decks.addAsset(d.id, n, 'image/png', new Uint8Array(randomBytes(64)));
    await expect(decks.save(d.id)).rejects.toThrow(/dropped/);
    await new Promise((r) => setTimeout(r, 100));
    expect([(await decks.state(d.id)).saving, seen.at(-1)]).toEqual([null, null]);
    expect((await decks.save(d.id)).dirty).toBe(false);
  });

  it('opens a deck from its datapack once, however many ask for it at once', async () => {
    const decks = new DeckStore(getPaths(join(dir, 'one')), meko);
    const d = await decks.create({ title: 'Shared', brief: emptyBrief(), sources: { content: [], template: null, animations: [] } });
    await decks.save(d.id);
    const elsewhere = new DeckStore(getPaths(join(dir, 'two')), meko);
    const [a, b] = await Promise.all([elsewhere.entry(d.id), elsewhere.entry(d.id)]);
    expect(a).toBe(b);
  });

  it('fetches a damaged piece again, then says so', async () => {
    const s = store();
    const saved = await s.save('dp1', sampleDeck(), new Set(), {});
    const scene = meko.puts.find((p) => p.filename === 'scenes/menu.json')!.hash;
    meko.damaged.add(scene);
    await expect(store('fresh').load('dp1', saved.manifestHash)).rejects.toThrow(/came back damaged twice/);
  });

  it('points deck.json at a manifest too big for one piece', async () => {
    const s = store();
    let deck = sampleDeck();
    const intent = 'x'.repeat(600);
    deck = applyOps(deck, Array.from({ length: 2000 }, (_, i) => ({ op: 'addScene' as const, scene: { id: `q${i}`, title: `Question ${i}`, intent, slides: [], detour: true } })));
    const saved = await s.save('dp1', deck, new Set(), {});
    const root = bytesJson((await meko.getArtifact('dp1', saved.manifestHash))!) as { format: string; manifest: { size: number } };
    expect(root.format).toBe(POINTER_FORMAT);
    expect(root.manifest.size).toBeGreaterThan(INDEX_BYTES);
    expect((await store('fresh').load('dp1', saved.manifestHash)).deck).toEqual(deck);
  });
});
