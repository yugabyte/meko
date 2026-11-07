import { createHash } from 'node:crypto';
import { existsSync, mkdtempSync, realpathSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { DatapackRow, MekoAt } from '../src/shared/datapacks.js';
import { newDeck, emptyBrief, type Deck, type Scene, type Slide } from '../src/shared/deck.js';
import { MekoError, type ArtifactRow, type MekoPort } from '../src/server/meko.js';

export const FAKE_CLAUDE = fileURLToPath(new URL('./fixtures/fake-claude.mjs', import.meta.url));

/**
 * A short temp folder (unix sockets have a length limit): under /tmp, since macOS's own temp folder
 * (/var/folders/…/T) is long enough to push a home's socket past it.
 */
export function tempDir(prefix = 'pres-'): string {
  return realpathSync(mkdtempSync(join(existsSync('/tmp') ? '/tmp' : tmpdir(), prefix)));
}

interface Stored {
  hash: string;
  filename: string;
  type: string;
  bytes: Uint8Array;
  createdAt: string;
}

/**
 * Meko for tests: datapacks and artifacts in memory, kept as Meko keeps them (by SHA-256, the same
 * bytes once per datapack, at most 5 MiB), with a way to fail after some uploads and to damage a
 * stored artifact.
 */
export class FakeMeko implements MekoPort {
  datapacks: DatapackRow[] = [];
  artifacts = new Map<string, Stored[]>();
  puts: { datapack: string; filename: string; hash: string }[] = [];
  /** Uploads left before artifact_put starts failing; Infinity never fails. */
  failAfter = Infinity;
  /** Hashes whose bytes come back wrong. */
  damaged = new Set<string>();
  /** The Meko it stands for; set it to another to stand for Meko after its link changed. */
  where: MekoAt | null = { kind: 'local', host: '127.0.0.1:7778' };
  /**
   * The datapacks the Meko on this computer serves itself while it links Meko Cloud;
   * null while Present doesn't reach Meko Cloud through it.
   */
  here: Set<string> | null = null;
  private clock = Date.parse('2026-11-01T10:00:00Z');

  async status() {
    return 'ready' as const;
  }

  async at(): Promise<MekoAt | null> {
    return this.where;
  }

  async listDatapacks(): Promise<DatapackRow[]> {
    return [...this.datapacks];
  }

  async createDatapack(name: string, description: string): Promise<DatapackRow> {
    if (name.length < 3 || name.length > 60) throw new MekoError('VALIDATION_ERROR: name must be 3 to 60 characters');
    if (this.datapacks.some((d) => d.name === name)) throw new MekoError('CONFLICT: the name is taken');
    const row = { id: `dp${this.datapacks.length + 1}`, name, description };
    this.datapacks.push(row);
    return row;
  }

  async putArtifact(datapackId: string, filename: string, bytes: Uint8Array, type: string): Promise<string> {
    if (this.failAfter <= 0) throw new MekoError('the connection dropped');
    this.failAfter--;
    if (bytes.length > 5 * 1024 * 1024) throw new MekoError('artifact_too_large: The file is larger than 5 MiB');
    const hash = createHash('sha256').update(bytes).digest('hex');
    const list = this.artifacts.get(datapackId) ?? [];
    this.puts.push({ datapack: datapackId, filename, hash });
    if (!list.some((a) => a.hash === hash)) {
      list.push({ hash, filename, type, bytes: new Uint8Array(bytes), createdAt: new Date(this.clock += 1000).toISOString() });
    }
    this.artifacts.set(datapackId, list);
    return hash;
  }

  async getArtifact(datapackId: string, hash: string): Promise<Uint8Array | null> {
    const a = this.artifacts.get(datapackId)?.find((x) => x.hash === hash);
    if (!a) return null;
    if (this.damaged.has(hash)) return new Uint8Array([...a.bytes.slice(0, -1), 0]);
    return a.bytes;
  }

  async localIds(): Promise<Set<string> | null> {
    return this.here && new Set(this.here);
  }

  async listArtifacts(datapackId: string, filename: string): Promise<ArtifactRow[]> {
    return (this.artifacts.get(datapackId) ?? [])
      .filter((a) => a.filename === filename)
      .map((a) => ({ hash: a.hash, filename: a.filename, size: a.bytes.length, createdAt: a.createdAt }))
      .sort((a, b) => b.createdAt.localeCompare(a.createdAt));
  }
}

export function slide(id: string, blocks: Slide['variants'][string]['blocks'] = [{ type: 'heading', text: id }], extra: Partial<Slide> = {}): Slide {
  return { id, layout: 'bullets', variants: { overview: { blocks } }, notes: '', sources: [], ...extra };
}

/**
 * The sample YugabyteDB pitch: an opening, a product fork with three choices, xCluster vs. sync
 * stretch behind a second fork, a recap that returns to the menu, and a pricing detour.
 */
export function sampleDeck(): Deck {
  const d = newDeck('YugabyteDB for Acme', { ...emptyBrief(), audience: 'Acme platform team' }, { content: ['ybdb/xcluster'], template: null, animations: [] });
  const scenes: [string, string, Partial<Scene>, string[]][] = [
    ['menu', 'What matters most?', { fork: true }, ['s-menu']],
    ['ybdb', 'YugabyteDB', {}, ['s-ybdb-1']],
    ['amp', 'AMP serverless', {}, ['s-amp-1']],
    ['meko', 'Meko and AgentK', {}, ['s-meko-1']],
    ['geo', 'Latency or zero loss?', { fork: true }, ['s-geo']],
    ['xcluster', 'xCluster async', {}, ['s-x-1', 's-x-2']],
    ['sync', 'Sync stretch', {}, ['s-sync-1']],
    ['recap', 'Tradeoffs recap', {}, ['s-recap']],
    ['pricing', 'Pricing', { detour: true }, ['s-price']],
  ];
  for (const [id, title, extra, slides] of scenes) {
    d.scenes[id] = { id, title, intent: '', slides, ...extra };
    for (const s of slides) d.slides[s] = slide(s);
  }
  d.slides['s-x-1'] = slide('s-x-1', [{ type: 'heading', text: 'Async replication' }, { type: 'bullets', items: ['one', 'two', 'three'], build: true }]);
  d.edges = [
    { id: 'e1', from: 'opening', to: 'menu', kind: 'next' },
    { id: 'e2', from: 'menu', to: 'ybdb', kind: 'choice', label: 'Distributed SQL', key: '1' },
    { id: 'e3', from: 'menu', to: 'amp', kind: 'choice', label: 'Serverless', key: '2' },
    { id: 'e4', from: 'menu', to: 'meko', kind: 'choice', label: 'AI agents', key: '3' },
    { id: 'e5', from: 'ybdb', to: 'geo', kind: 'next' },
    { id: 'e6', from: 'geo', to: 'xcluster', kind: 'choice', label: 'Low write latency', key: '1' },
    { id: 'e7', from: 'geo', to: 'sync', kind: 'choice', label: 'Zero data loss', key: '2' },
    { id: 'e8', from: 'xcluster', to: 'recap', kind: 'next' },
    { id: 'e9', from: 'sync', to: 'recap', kind: 'next' },
    { id: 'e10', from: 'recap', to: 'menu', kind: 'return' },
    { id: 'e11', from: 'amp', to: 'menu', kind: 'return' },
    { id: 'e12', from: 'meko', to: 'menu', kind: 'return' },
  ];
  return d;
}

/** Writes the stub claude's answers: the first whose `match` is in the message wins. */
export function fakeAnswers(dir: string, answers: { match: string; answer: unknown; files?: Record<string, string>; error?: string }[]): string {
  const f = join(dir, 'answers.json');
  writeFileSync(f, JSON.stringify(answers));
  return f;
}
