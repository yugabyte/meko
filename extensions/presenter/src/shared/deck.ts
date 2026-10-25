// The deck: a graph of scenes (each a linear run of slides) joined by next, choice and return
// edges, with slides made of structured blocks. One module, no Node or DOM imports: the server and
// the pages both use it, so the editor previews and undoes with the same rules the server checks.

export const DECK_FORMAT = 'presenter-deck';
export const DECK_VERSION = 1;

/** The most one asset can be, and all of a deck's assets together (its template's included). */
export const MAX_ASSET_BYTES = 100 * 1024 * 1024;
export const MAX_DECK_BYTES = 1024 * 1024 * 1024;

export type Shape = 'linear' | 'tree' | 'graph';
export const SHAPES: Shape[] = ['linear', 'tree', 'graph'];

export interface Brief {
  audience: string;
  goal: string;
  minutes: number;
  shape: Shape;
  notes: string;
}

/** The datapacks a deck is built from, by name. Content can be any datapack. */
export interface DeckSources {
  content: string[];
  template: string | null;
  animations: string[];
}

export interface Scene {
  id: string;
  title: string;
  /** What the scene covers, for the map and for Claude. */
  intent: string;
  slides: string[];
  /** Ends on a question; its outgoing edges are the choices. */
  fork?: boolean;
  /** Reached only by a jump; the talk returns to where it was after it. */
  detour?: boolean;
  /** Where the map draws it. */
  x?: number;
  y?: number;
}

export type EdgeKind = 'next' | 'choice' | 'return';
export const EDGE_KINDS: EdgeKind[] = ['next', 'choice', 'return'];

export interface Edge {
  id: string;
  from: string;
  to: string;
  kind: EdgeKind;
  /** A choice's option, as the fork's question slide shows it. */
  label?: string;
  /** The key that takes a choice on the console: '1' to '9'. */
  key?: string;
}

export type Block =
  | { type: 'heading'; text: string }
  | { type: 'text'; markdown: string }
  | { type: 'bullets'; items: string[]; build?: boolean }
  | { type: 'metric'; value: string; label: string }
  | { type: 'quote'; text: string; by?: string }
  | { type: 'table'; header: string[]; rows: string[][] }
  | { type: 'image'; asset: string; alt: string }
  | { type: 'video'; asset: string; caption?: string }
  | { type: 'diagram'; mermaid: string }
  | { type: 'animation'; ref: string; params: Record<string, unknown> }
  | { type: 'code'; code: string; language?: string };

export type BlockType = Block['type'];
export const BLOCK_TYPES: BlockType[] = ['heading', 'text', 'bullets', 'metric', 'quote', 'table', 'image', 'video', 'diagram', 'animation', 'code'];

export type Layout = 'title' | 'section' | 'bullets' | 'two-column' | 'diagram-right' | 'big-number' | 'quote' | 'full';
export const LAYOUTS: Layout[] = ['title', 'section', 'bullets', 'two-column', 'diagram-right', 'big-number', 'quote', 'full'];

export interface SlideVariant {
  blocks: Block[];
}

export interface Source {
  kind: 'memory' | 'knowledge' | 'web';
  datapack?: string;
  id?: string;
  url?: string;
  excerpt?: string;
}

export interface Slide {
  id: string;
  layout: Layout;
  /** By variant name; every slide has the deck's first variant. */
  variants: Record<string, SlideVariant>;
  notes: string;
  sources: Source[];
  /** The author changed it by hand: Claude's change sets leave it unticked. */
  handEdited?: boolean;
}

/** A file a slide shows: kept by its SHA-256, with its size and media type. */
export interface AssetRef {
  hash: string;
  size: number;
  type: string;
}

export interface Deck {
  format: typeof DECK_FORMAT;
  version: typeof DECK_VERSION;
  title: string;
  brief: Brief;
  sources: DeckSources;
  /** Variant names, the default first: say ["overview", "deep"]. */
  variants: string[];
  start: string;
  scenes: Record<string, Scene>;
  edges: Edge[];
  slides: Record<string, Slide>;
  assets: Record<string, AssetRef>;
}

/** A short random id: scene, slide and edge ids are made in the browser and on the server alike. */
export function newId(prefix: string): string {
  const bytes = new Uint8Array(6);
  globalThis.crypto.getRandomValues(bytes);
  return `${prefix}-${Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('')}`;
}

export function emptyBrief(): Brief {
  return { audience: '', goal: '', minutes: 30, shape: 'graph', notes: '' };
}

/** A new deck: one title scene with one title slide. */
export function newDeck(title: string, brief: Brief, sources: DeckSources): Deck {
  const scene = 'opening';
  const slide = newId('s');
  return {
    format: DECK_FORMAT,
    version: DECK_VERSION,
    title,
    brief,
    sources,
    variants: ['overview', 'deep'],
    start: scene,
    scenes: { [scene]: { id: scene, title, intent: brief.goal, slides: [slide], x: 0, y: 0 } },
    edges: [],
    slides: {
      [slide]: {
        id: slide,
        layout: 'title',
        variants: { overview: { blocks: [{ type: 'heading', text: title }, ...(brief.audience ? [{ type: 'text' as const, markdown: `For ${brief.audience}` }] : [])] } },
        notes: '',
        sources: [],
      },
    },
    assets: {},
  };
}

export function outgoing(deck: Deck, scene: string): Edge[] {
  return deck.edges.filter((e) => e.from === scene);
}

/** A fork's choices, in key order. */
export function choices(deck: Deck, scene: string): Edge[] {
  return outgoing(deck, scene)
    .filter((e) => e.kind === 'choice')
    .sort((a, b) => String(a.key ?? '').localeCompare(String(b.key ?? '')));
}

/** A slide's blocks in a variant, falling back to the deck's first. */
export function blocksOf(deck: Deck, slide: Slide, variant: string): Block[] {
  return (own(slide.variants, variant) ?? own(slide.variants, deck.variants[0]!) ?? { blocks: [] }).blocks;
}

/**
 * A deck's map entry by key, only when the deck has that key itself: a plain object also answers
 * for inherited names such as "constructor" or "toString", which are no scene, slide or asset.
 */
export function own<T>(map: Record<string, T>, key: string): T | undefined {
  return Object.hasOwn(map, key) ? map[key] : undefined;
}

/** What an asset may be named: its file in the deck's datapack is assets/<name>. */
export const ASSET_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$/;

/** Bytes of every asset the deck lists. */
export function assetBytes(deck: Deck): number {
  return Object.values(deck.assets).reduce((n, a) => n + a.size, 0);
}

/** Whether one more asset of this size fits: why not, or null. */
export function assetFits(deck: Deck, size: number, replacing?: string): string | null {
  const mib = (n: number) => `${Math.round((n / 1024 / 1024) * 10) / 10} MiB`;
  if (size > MAX_ASSET_BYTES) return `The file is bigger than ${mib(MAX_ASSET_BYTES)}, the most one asset can be. Trim or compress it first.`;
  const used = assetBytes(deck) - (replacing ? (own(deck.assets, replacing)?.size ?? 0) : 0);
  if (used + size > MAX_DECK_BYTES) return `The file is ${mib(size)}, and the deck has ${mib(Math.max(0, MAX_DECK_BYTES - used))} of its ${mib(MAX_DECK_BYTES)} left.`;
  return null;
}

// ---- checking ----

const isObj = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);
const isStr = (v: unknown): v is string => typeof v === 'string';
const isStrArr = (v: unknown): v is string[] => Array.isArray(v) && v.every(isStr);
const isBool = (v: unknown): v is boolean => typeof v === 'boolean';
const isNum = (v: unknown): v is number => typeof v === 'number' && Number.isFinite(v);
/** An optional field: absent, or of its type. The pages show these as text, so an object would break them. */
const optional = (v: unknown, is: (v: unknown) => boolean) => v === undefined || is(v);
const HASH = /^[0-9a-f]{64}$/;
const ANIM_REF = /^[^:\s]+:[A-Za-z0-9._-]+$/;
const SOURCE_KINDS = ['memory', 'knowledge', 'web'];

/** What's wrong with a block, or null. */
function blockProblem(b: unknown, deck: { assets: Record<string, unknown> }): string | null {
  if (!isObj(b) || !isStr(b.type)) return 'a block has no type';
  switch (b.type) {
    case 'heading': return isStr(b.text) ? null : 'a heading has no text';
    case 'text': return isStr(b.markdown) ? null : 'a text block has no markdown';
    case 'bullets':
      if (!isStrArr(b.items)) return 'a bullets block needs items, a list of strings';
      return optional(b.build, isBool) ? null : 'a bullets block\'s build is true or false';
    case 'metric': return isStr(b.value) && isStr(b.label) ? null : 'a metric needs a value and a label';
    case 'quote': return isStr(b.text) && optional(b.by, isStr) ? null : 'a quote needs its text, and who said it (by) as text';
    case 'table':
      return isStrArr(b.header) && Array.isArray(b.rows) && b.rows.every(isStrArr) ? null : 'a table needs a header and rows of strings';
    case 'image':
    case 'video':
      if (!isStr(b.asset)) return `an ${b.type} block names no asset`;
      if (!Object.hasOwn(deck.assets, b.asset)) return `the ${b.type} block names asset "${b.asset}", which the deck doesn't have`;
      return optional(b.type === 'image' ? b.alt : b.caption, isStr) ? null : `an ${b.type} block's ${b.type === 'image' ? 'alt' : 'caption'} isn't text`;
    case 'diagram': return isStr(b.mermaid) ? null : 'a diagram has no mermaid text';
    case 'animation':
      if (!isStr(b.ref) || !ANIM_REF.test(b.ref)) return 'an animation block needs a ref, "<datapack>:<name>"';
      return isObj(b.params) ? null : 'an animation block needs params, an object';
    case 'code': return isStr(b.code) && optional(b.language, isStr) ? null : 'a code block needs its code, and its language as text';
    default: return `"${b.type}" isn't a block type`;
  }
}

/** Whether a slide's source is {kind: memory, knowledge or web} with only text in its other fields. */
function sourceOk(s: unknown): boolean {
  return isObj(s) && SOURCE_KINDS.includes(s.kind as string) && ['datapack', 'id', 'url', 'excerpt'].every((k) => optional(s[k], isStr));
}

/**
 * Checks a deck from anywhere (the server's draft, Meko, Claude's ops applied): its shapes, and its
 * graph. The start exists, every edge names real scenes, every scene but a detour is reachable from
 * the start, a fork has two or more choices with distinct labels and keys, only a fork has choices,
 * every slide is in exactly one scene and has the deck's first variant, and every asset a block
 * names is in the deck within the size limits. An empty list is a good deck.
 */
export function deckProblems(x: unknown): string[] {
  if (!isObj(x)) return ['The deck isn\'t an object.'];
  const p: string[] = [];
  if (x.format !== DECK_FORMAT || x.version !== DECK_VERSION) p.push(`The deck's format isn't ${DECK_FORMAT} version ${DECK_VERSION}.`);
  if (!isStr(x.title) || !x.title.trim()) p.push('The deck has no title.');
  if (!isStrArr(x.variants) || x.variants.length === 0) p.push('The deck names no variants.');
  if (!isObj(x.scenes) || !isObj(x.slides) || !Array.isArray(x.edges) || !isObj(x.assets) || !isObj(x.brief) || !isObj(x.sources)) {
    p.push('The deck is missing its scenes, slides, edges, assets, brief or sources.');
    return p;
  }
  const deck = x as unknown as Deck;
  const first = deck.variants?.[0];
  // An object's own "__proto__" key (JSON can make one) can't be set or read back like other ids.
  if ([deck.scenes, deck.slides, deck.assets].some((m) => Object.hasOwn(m, '__proto__'))) p.push('A scene, slide or asset is named "__proto__". Rename it.');

  for (const [name, a] of Object.entries(deck.assets)) {
    if (!ASSET_NAME.test(name)) p.push(`Asset "${name}" needs a name of letters, digits, dots, dashes and underscores.`);
    if (!isObj(a) || !isStr(a.hash) || !HASH.test(a.hash) || typeof a.size !== 'number' || !isStr(a.type)) p.push(`Asset "${name}" needs a SHA-256 hash, a size and a type.`);
    else if (a.size > MAX_ASSET_BYTES) p.push(`Asset "${name}" is over the ${MAX_ASSET_BYTES / 1024 / 1024} MiB limit.`);
  }
  if (assetBytes(deck) > MAX_DECK_BYTES) p.push('The deck\'s assets are over the 1 GiB limit.');

  const inScene = new Map<string, string>();
  for (const [id, s] of Object.entries(deck.scenes)) {
    if (!isObj(s) || s.id !== id || !isStr(s.title) || !isStrArr(s.slides)) {
      p.push(`Scene "${id}" needs its id, a title and a list of slides.`);
      continue;
    }
    if (!optional(s.intent, isStr)) p.push(`Scene "${s.title}" has an intent that isn't text.`);
    if (!optional(s.fork, isBool) || !optional(s.detour, isBool)) p.push(`Scene "${s.title}" has a fork or detour that isn't true or false.`);
    if (!optional(s.x, isNum) || !optional(s.y, isNum)) p.push(`Scene "${s.title}" has a place on the map (x, y) that isn't a number.`);
    for (const sl of s.slides) {
      if (!Object.hasOwn(deck.slides, sl)) p.push(`Scene "${s.title}" lists slide "${sl}", which the deck doesn't have.`);
      else if (inScene.has(sl)) p.push(`Slide "${sl}" is in two scenes.`);
      else inScene.set(sl, id);
    }
  }
  for (const [id, sl] of Object.entries(deck.slides)) {
    if (!isObj(sl) || sl.id !== id || !isObj(sl.variants) || !isStr(sl.layout)) {
      p.push(`Slide "${id}" needs its id, a layout and variants.`);
      continue;
    }
    if (!LAYOUTS.includes(sl.layout)) p.push(`Slide "${id}" has layout "${sl.layout}", which isn't one of ${LAYOUTS.join(', ')}.`);
    if (!inScene.has(id)) p.push(`Slide "${id}" isn't in any scene.`);
    if (first && !Object.hasOwn(sl.variants, first)) p.push(`Slide "${id}" has no "${first}" variant, the deck's first.`);
    for (const [v, body] of Object.entries(sl.variants)) {
      if (!isObj(body) || !Array.isArray(body.blocks)) {
        p.push(`Slide "${id}" variant "${v}" has no blocks.`);
        continue;
      }
      for (const b of body.blocks) {
        const bp = blockProblem(b, deck);
        if (bp) p.push(`Slide "${id}" (${v}): ${bp}.`);
      }
    }
    if (!optional(sl.notes, isStr)) p.push(`Slide "${id}" has notes that aren't text.`);
    if (!optional(sl.handEdited, isBool)) p.push(`Slide "${id}" has a handEdited that isn't true or false.`);
    if (!Array.isArray(sl.sources)) p.push(`Slide "${id}" has no sources list.`);
    else if (!sl.sources.every(sourceOk)) p.push(`Slide "${id}" has a source that isn't {kind: memory, knowledge or web} with text fields.`);
  }

  if (!isStr(deck.start) || !Object.hasOwn(deck.scenes, deck.start)) p.push('The deck\'s start isn\'t one of its scenes.');
  const ids = new Set<string>();
  for (const e of deck.edges) {
    if (!isObj(e) || !isStr(e.id) || !isStr(e.from) || !isStr(e.to) || !EDGE_KINDS.includes(e.kind as EdgeKind)) {
      p.push('An edge needs an id, from, to and a kind (next, choice or return).');
      continue;
    }
    if (ids.has(e.id)) p.push(`Two edges have id "${e.id}".`);
    ids.add(e.id);
    if (!Object.hasOwn(deck.scenes, e.from) || !Object.hasOwn(deck.scenes, e.to)) p.push(`Edge "${e.id}" names a scene the deck doesn't have.`);
    if (!optional(e.label, isStr) || !optional(e.key, isStr)) p.push(`Edge "${e.id}" has a label or key that isn't text.`);
    if (e.kind === 'choice' && !own(deck.scenes, e.from)?.fork) p.push(`Edge "${e.id}" is a choice from "${e.from}", which isn't a fork.`);
  }
  for (const s of Object.values(deck.scenes)) {
    if (!isObj(s) || !s.fork) continue;
    const cs = choices(deck, s.id);
    if (cs.length < 2) p.push(`Fork "${s.title}" needs two or more choices: connect it to the scenes it branches to.`);
    const labels = cs.map((c) => String(c.label ?? '').trim().toLowerCase());
    const keys = cs.map((c) => String(c.key ?? ''));
    if (labels.some((l) => !l) || new Set(labels).size !== labels.length) p.push(`Fork "${s.title}" needs a distinct label on each choice.`);
    if (keys.some((k) => !/^[1-9]$/.test(k)) || new Set(keys).size !== keys.length) p.push(`Fork "${s.title}" needs a distinct key, 1 to 9, on each choice.`);
  }
  if (p.length === 0) {
    const seen = new Set<string>([deck.start]);
    const queue = [deck.start];
    while (queue.length) {
      const at = queue.shift()!;
      for (const e of outgoing(deck, at)) {
        if (seen.has(e.to)) continue;
        seen.add(e.to);
        queue.push(e.to);
      }
    }
    for (const s of Object.values(deck.scenes)) {
      if (!s.detour && !seen.has(s.id)) p.push(`Scene "${s.title}" can't be reached from the start. Connect it, or make it a detour.`);
    }
  }
  return p;
}

export class DeckError extends Error {
  constructor(readonly problems: string[]) {
    super(problems.join(' '));
  }
}

/** The deck, or a DeckError listing what's wrong with it. */
export function checkDeck(x: unknown): Deck {
  const p = deckProblems(x);
  if (p.length) throw new DeckError(p);
  return x as Deck;
}
