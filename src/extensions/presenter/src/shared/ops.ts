// Every change to a deck, by hand or by Claude, is a list of typed operations: the editor sends them
// for hand edits, Claude returns them for asks and builds. applyOps applies a list and the deck's
// checks decide whether the result stands, so both paths share one undo history and one set of rules.

import { ASSET_NAME, checkDeck, own, type AssetRef, type Deck, type Edge, type Scene, type Slide } from './deck.js';

export type Op =
  | { op: 'addScene'; scene: Scene }
  | { op: 'updateScene'; id: string; patch: Partial<Omit<Scene, 'id' | 'slides'>> }
  | { op: 'removeScene'; id: string }
  | { op: 'connect'; edge: Edge }
  | { op: 'disconnect'; id: string }
  | { op: 'updateEdge'; id: string; patch: Partial<Omit<Edge, 'id'>> }
  | { op: 'addSlide'; scene: string; slide: Slide; index?: number }
  | { op: 'updateSlide'; slide: Slide }
  | { op: 'moveSlide'; id: string; scene: string; index: number }
  | { op: 'removeSlide'; id: string }
  | { op: 'setStart'; scene: string }
  | { op: 'setTitle'; title: string }
  | { op: 'setVariants'; variants: string[] }
  | { op: 'addAsset'; name: string; asset: AssetRef };

export type OpName = Op['op'];
export const OP_NAMES: OpName[] = [
  'addScene', 'updateScene', 'removeScene', 'connect', 'disconnect', 'updateEdge',
  'addSlide', 'updateSlide', 'moveSlide', 'removeSlide', 'setStart', 'setTitle', 'setVariants', 'addAsset',
];

export class OpError extends Error {
  constructor(readonly index: number, message: string) {
    super(`Change ${index + 1}: ${message}`);
  }
}

const need = <T>(v: T | undefined, i: number, what: string): T => {
  if (v === undefined) throw new OpError(i, what);
  return v;
};

/** A new scene's or slide's id: text, and not "__proto__", which an object can't hold as a key. */
const freshId = (id: unknown, i: number, what: string): string => {
  if (typeof id !== 'string' || !id || id === '__proto__') throw new OpError(i, `a new ${what} needs an id.`);
  return id;
};

/** An update's patch: an object of the fields to change. */
const patchOf = <T>(p: unknown, i: number): T => {
  if (!p || typeof p !== 'object' || Array.isArray(p)) throw new OpError(i, 'an update needs a patch, an object of the fields to change.');
  return p as T;
};

function applyOne(d: Deck, o: Op, i: number): void {
  if (!o || typeof o !== 'object' || !OP_NAMES.includes((o as { op: OpName }).op)) throw new OpError(i, `"${String((o as { op?: unknown })?.op)}" isn't a change Presenter knows.`);
  switch (o.op) {
    case 'addScene': {
      const id = freshId(o.scene?.id, i, 'scene');
      if (Object.hasOwn(d.scenes, id)) throw new OpError(i, `there's already a scene "${id}".`);
      // A new scene starts empty: its slides come with addSlide.
      d.scenes[id] = { ...o.scene, slides: [] };
      return;
    }
    case 'updateScene': {
      const s = need(own(d.scenes, o.id), i, `there's no scene "${o.id}".`);
      const { id: _id, slides: _slides, ...patch } = patchOf<Partial<Scene>>(o.patch, i);
      d.scenes[o.id] = { ...s, ...patch };
      return;
    }
    case 'removeScene': {
      const s = need(own(d.scenes, o.id), i, `there's no scene "${o.id}".`);
      for (const sl of s.slides) delete d.slides[sl];
      delete d.scenes[o.id];
      d.edges = d.edges.filter((e) => e.from !== o.id && e.to !== o.id);
      return;
    }
    case 'connect':
      if (!o.edge?.id) throw new OpError(i, 'a new edge needs an id.');
      if (d.edges.some((e) => e.id === o.edge.id)) throw new OpError(i, `there's already an edge "${o.edge.id}".`);
      d.edges.push({ ...o.edge });
      return;
    case 'disconnect':
      if (!d.edges.some((e) => e.id === o.id)) throw new OpError(i, `there's no edge "${o.id}".`);
      d.edges = d.edges.filter((e) => e.id !== o.id);
      return;
    case 'updateEdge': {
      const at = d.edges.findIndex((e) => e.id === o.id);
      if (at < 0) throw new OpError(i, `there's no edge "${o.id}".`);
      d.edges[at] = { ...d.edges[at]!, ...patchOf<Partial<Edge>>(o.patch, i), id: o.id };
      return;
    }
    case 'addSlide': {
      const s = need(own(d.scenes, o.scene), i, `there's no scene "${o.scene}".`);
      const id = freshId(o.slide?.id, i, 'slide');
      if (Object.hasOwn(d.slides, id)) throw new OpError(i, `there's already a slide "${id}".`);
      d.slides[id] = o.slide;
      const at = o.index === undefined ? s.slides.length : Math.max(0, Math.min(o.index, s.slides.length));
      s.slides.splice(at, 0, id);
      return;
    }
    case 'updateSlide':
      need(own(d.slides, o.slide?.id), i, `there's no slide "${o.slide?.id}".`);
      d.slides[o.slide.id] = o.slide;
      return;
    case 'moveSlide': {
      need(own(d.slides, o.id), i, `there's no slide "${o.id}".`);
      const to = need(own(d.scenes, o.scene), i, `there's no scene "${o.scene}".`);
      for (const s of Object.values(d.scenes)) s.slides = s.slides.filter((x) => x !== o.id);
      to.slides.splice(Math.max(0, Math.min(o.index, to.slides.length)), 0, o.id);
      return;
    }
    case 'removeSlide':
      need(own(d.slides, o.id), i, `there's no slide "${o.id}".`);
      delete d.slides[o.id];
      for (const s of Object.values(d.scenes)) s.slides = s.slides.filter((x) => x !== o.id);
      return;
    case 'setStart':
      need(own(d.scenes, o.scene), i, `there's no scene "${o.scene}".`);
      d.start = o.scene;
      return;
    case 'setTitle':
      d.title = String(o.title ?? '').trim();
      return;
    case 'setVariants':
      if (!Array.isArray(o.variants)) throw new OpError(i, 'variants is a list of names.');
      d.variants = [...o.variants];
      return;
    case 'addAsset':
      if (typeof o.name !== 'string' || !ASSET_NAME.test(o.name)) throw new OpError(i, `asset "${String(o.name)}" needs a name of letters, digits, dots, dashes and underscores.`);
      d.assets[o.name] = o.asset;
      return;
  }
}

/**
 * The deck after the operations, checked: the input is never changed. Throws an OpError for an
 * operation that can't apply, or a DeckError when the result breaks the deck's rules.
 */
export function applyOps(deck: Deck, ops: Op[]): Deck {
  const d = structuredClone(deck);
  ops.forEach((o, i) => applyOne(d, o, i));
  return checkDeck(d);
}

/** applyOps, as a result instead of a throw. */
export function tryOps(deck: Deck, ops: Op[]): { ok: true; deck: Deck } | { ok: false; problems: string[] } {
  try {
    return { ok: true, deck: applyOps(deck, ops) };
  } catch (e) {
    const problems = (e as { problems?: string[] }).problems;
    return { ok: false, problems: problems ?? [e instanceof Error ? e.message : String(e)] };
  }
}

/** One line for a change set: + adds, - removes, ~ changes. */
export interface OpLine {
  sym: '+' | '-' | '~';
  text: string;
  /** Rewrites or removes a slide the author edited by hand: unticked by default. */
  handEdited: boolean;
}

export function describeOp(deck: Deck, o: Op): OpLine {
  const sceneName = (id: string) => own(deck.scenes, id)?.title ?? id;
  const slideName = (id: string) => {
    const scene = Object.values(deck.scenes).find((s) => s.slides.includes(id));
    return scene ? `${scene.title} · ${scene.slides.indexOf(id) + 1}` : id;
  };
  const hand = (id: string) => !!own(deck.slides, id)?.handEdited;
  switch (o.op) {
    case 'addScene': return { sym: '+', text: `Scene "${o.scene.title}"${o.scene.fork ? ' (fork)' : o.scene.detour ? ' (detour)' : ''}`, handEdited: false };
    case 'updateScene': return { sym: '~', text: `Scene "${sceneName(o.id)}": ${Object.keys(o.patch ?? {}).join(', ')}`, handEdited: false };
    case 'removeScene': return { sym: '-', text: `Scene "${sceneName(o.id)}"`, handEdited: (deck.scenes[o.id]?.slides ?? []).some(hand) };
    case 'connect': return { sym: '+', text: `${o.edge.kind === 'choice' ? `Choice "${o.edge.label ?? ''}"` : o.edge.kind === 'return' ? 'Return' : 'Next'}: ${sceneName(o.edge.from)} → ${sceneName(o.edge.to)}`, handEdited: false };
    case 'disconnect': {
      const e = deck.edges.find((x) => x.id === o.id);
      return { sym: '-', text: e ? `Edge ${sceneName(e.from)} → ${sceneName(e.to)}` : `Edge ${o.id}`, handEdited: false };
    }
    case 'updateEdge': {
      const e = deck.edges.find((x) => x.id === o.id);
      return { sym: '~', text: e ? `Edge ${sceneName(e.from)} → ${sceneName(o.patch?.to ?? e.to)}` : `Edge ${o.id}`, handEdited: false };
    }
    case 'addSlide': return { sym: '+', text: `Slide in "${sceneName(o.scene)}"`, handEdited: false };
    case 'updateSlide': return { sym: '~', text: `Slide ${slideName(o.slide.id)}`, handEdited: hand(o.slide.id) };
    case 'moveSlide': return { sym: '~', text: `Move slide ${slideName(o.id)} to "${sceneName(o.scene)}"`, handEdited: false };
    case 'removeSlide': return { sym: '-', text: `Slide ${slideName(o.id)}`, handEdited: hand(o.id) };
    case 'setStart': return { sym: '~', text: `Start at "${sceneName(o.scene)}"`, handEdited: false };
    case 'setTitle': return { sym: '~', text: `Title "${o.title}"`, handEdited: false };
    case 'setVariants': return { sym: '~', text: `Variants ${o.variants.join(', ')}`, handEdited: false };
    case 'addAsset': {
      // A new file under a name a hand-edited slide shows changes that slide too.
      const was = own(deck.assets, o.name);
      const replaces = !!was && was.hash !== o.asset?.hash;
      const shown = (s: Slide) => Object.values(s.variants).some((v) => v.blocks.some((b) => (b.type === 'image' || b.type === 'video') && b.asset === o.name));
      return {
        sym: replaces ? '~' : '+', text: `${replaces ? 'New file for asset' : 'Asset'} ${o.name}`,
        handEdited: replaces && Object.values(deck.slides).some((s) => !!s.handEdited && shown(s)),
      };
    }
  }
}

/** Which of a change set's operations start ticked: all but the ones that overwrite hand edits. */
export function defaultTicks(deck: Deck, ops: Op[]): boolean[] {
  return ops.map((o) => !describeOp(deck, o).handEdited);
}
