// A live run of a deck: where the talk is, and how each console action moves it. Pure, so the server
// (which holds the run) and the console (which previews "next if 1") agree.

import { blocksOf, choices, outgoing, own, type Deck } from './deck.js';

export interface RunPos {
  scene: string;
  slide: number;
  /** How many build steps of the slide are shown (bullets with build on, one item a step). */
  step: number;
}

/** A place Back returns to: the position, and the way back from jumps as it was there. */
export interface RunStep {
  pos: RunPos;
  trail: RunPos[];
}

export interface RunState {
  id: string;
  deckId: string;
  variant: string;
  pos: RunPos;
  /** Where to go back to after a jump or a detour, innermost last. */
  trail: RunPos[];
  /** Where the talk was before each move to another scene, for Back. */
  back: RunStep[];
  /** Scenes shown so far. */
  visited: string[];
  blank: boolean;
  ended: boolean;
  startedAt: number;
}

export type NavAction =
  | { action: 'next' }
  | { action: 'back' }
  | { action: 'choose'; key: string }
  | { action: 'jump'; scene: string }
  | { action: 'return' }
  | { action: 'variant'; variant: string }
  | { action: 'blank'; on: boolean }
  | { action: 'restart' };

export function startRun(deck: Deck, id: string, deckId: string, variant: string, now: number): RunState {
  return {
    id, deckId, variant: deck.variants.includes(variant) ? variant : deck.variants[0]!,
    pos: { scene: deck.start, slide: 0, step: 0 }, trail: [], back: [], visited: [deck.start],
    blank: false, ended: false, startedAt: now,
  };
}

/** The build steps a slide has in a variant. */
export function stepsOf(deck: Deck, slideId: string | undefined, variant: string): number {
  const slide = slideId ? own(deck.slides, slideId) : undefined;
  if (!slide) return 0;
  return blocksOf(deck, slide, variant).reduce((n, b) => n + (b.type === 'bullets' && b.build ? b.items.length : 0), 0);
}

export function slideAt(deck: Deck, pos: RunPos): string | undefined {
  return own(deck.scenes, pos.scene)?.slides[pos.slide];
}

/** Whether the run is on the last slide of a fork, where the talk waits for a choice. */
export function atChoice(deck: Deck, r: RunState): boolean {
  const scene = own(deck.scenes, r.pos.scene);
  return !!scene?.fork && r.pos.slide >= scene.slides.length - 1;
}

/** The run moved to the start of another scene; Back comes back to where it was, trail and all. */
function enter(r: RunState, scene: string): RunState {
  return {
    ...r,
    back: [...r.back, { pos: r.pos, trail: r.trail }],
    pos: { scene, slide: 0, step: 0 },
    visited: r.visited.includes(scene) ? r.visited : [...r.visited, scene],
    ended: false,
  };
}

/** Where Next goes after a scene's last slide: its next edge, else its return edge; null at a fork or a dead end. */
export function afterScene(deck: Deck, scene: string): string | null {
  if (own(deck.scenes, scene)?.fork) return null;
  const out = outgoing(deck, scene);
  return (out.find((e) => e.kind === 'next') ?? out.find((e) => e.kind === 'return'))?.to ?? null;
}

/** The run after an action. An action that can't apply leaves it as it was. */
export function navigate(deck: Deck, r: RunState, a: NavAction): RunState {
  const scene = own(deck.scenes, r.pos.scene);
  if (!scene) return { ...r, pos: { scene: deck.start, slide: 0, step: 0 } };
  const steps = stepsOf(deck, slideAt(deck, r.pos), r.variant);
  switch (a.action) {
    case 'next': {
      if (r.pos.step < steps) return { ...r, pos: { ...r.pos, step: r.pos.step + 1 } };
      if (r.pos.slide < scene.slides.length - 1) return { ...r, pos: { ...r.pos, slide: r.pos.slide + 1, step: 0 } };
      if (scene.fork) return r;
      const to = afterScene(deck, scene.id);
      if (to) return enter(r, to);
      if (r.trail.length) return navigate(deck, r, { action: 'return' });
      return { ...r, ended: true };
    }
    case 'back': {
      if (r.ended) return { ...r, ended: false };
      if (r.pos.step > 0) return { ...r, pos: { ...r.pos, step: r.pos.step - 1 } };
      if (r.pos.slide > 0) {
        const prev = { ...r.pos, slide: r.pos.slide - 1, step: 0 };
        return { ...r, pos: { ...prev, step: stepsOf(deck, slideAt(deck, prev), r.variant) } };
      }
      // Back out of a jump forgets it; Back into a detour after its Return knows the way back again.
      const prev = r.back[r.back.length - 1];
      return prev ? { ...r, pos: prev.pos, trail: prev.trail, back: r.back.slice(0, -1) } : r;
    }
    case 'choose': {
      if (!scene.fork) return r;
      const edge = choices(deck, scene.id).find((e) => e.key === a.key);
      return edge ? enter(r, edge.to) : r;
    }
    case 'jump':
      if (!Object.hasOwn(deck.scenes, a.scene) || a.scene === r.pos.scene) return r;
      return { ...enter(r, a.scene), trail: [...r.trail, r.pos] };
    case 'return': {
      const to = r.trail[r.trail.length - 1];
      return to ? { ...r, back: [...r.back, { pos: r.pos, trail: r.trail }], pos: to, trail: r.trail.slice(0, -1), ended: false } : r;
    }
    case 'variant':
      return deck.variants.includes(a.variant) ? { ...r, variant: a.variant, pos: { ...r.pos, step: 0 } } : r;
    case 'blank':
      return { ...r, blank: a.on === true };
    case 'restart':
      return { ...startRun(deck, r.id, r.deckId, r.variant, r.startedAt) };
    default:
      // A message from a page that knows an action this server doesn't.
      return r;
  }
}

/** A position in the deck as it is now: its slide and step kept in range, or null when its scene is gone. */
function fitPos(deck: Deck, p: RunPos, variant: string): RunPos | null {
  const scene = own(deck.scenes, p.scene);
  if (!scene) return null;
  const slide = Math.min(p.slide, Math.max(0, scene.slides.length - 1));
  return { scene: scene.id, slide, step: Math.min(p.step, stepsOf(deck, scene.slides[slide], variant)) };
}

/**
 * The run checked against the deck, after an edit: a position that's gone moves to the nearest one
 * that isn't, and places in the trail and Back's steps whose scene is gone are dropped.
 */
export function fitRun(deck: Deck, r: RunState): RunState {
  const variant = deck.variants.includes(r.variant) ? r.variant : deck.variants[0]!;
  const fit = (ps: RunPos[]) => ps.flatMap((p) => fitPos(deck, p, variant) ?? []);
  return {
    ...r,
    variant,
    pos: fitPos(deck, r.pos, variant) ?? { scene: deck.start, slide: 0, step: 0 },
    trail: fit(r.trail),
    back: r.back.flatMap((b) => {
      const pos = fitPos(deck, b.pos, variant);
      return pos ? [{ pos, trail: fit(b.trail) }] : [];
    }),
  };
}
