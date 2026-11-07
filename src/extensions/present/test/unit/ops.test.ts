import { describe, expect, it } from 'vitest';
import { applyOps, defaultTicks, describeOp, tryOps, type Op } from '../../src/shared/ops.js';
import type { Deck } from '../../src/shared/deck.js';
import { sampleDeck, slide } from '../helpers.js';

describe('applyOps', () => {
  it.each<[string, Op[], (d: Deck) => unknown, unknown]>([
    ['adds a detour scene', [{ op: 'addScene', scene: { id: 'faq', title: 'FAQ', intent: '', slides: ['s-amp-1'], detour: true } }], (d) => d.scenes.faq?.slides, []],
    ['renames a scene, keeping its slides', [{ op: 'updateScene', id: 'amp', patch: { title: 'AMP', slides: [] } as never }], (d) => [d.scenes.amp!.title, d.scenes.amp!.slides], ['AMP', ['s-amp-1']]],
    ['removes a scene with its slides and edges', [{ op: 'removeScene', id: 'pricing' }], (d) => ['pricing' in d.scenes, 's-price' in d.slides], [false, false]],
    ['adds a slide where it\'s asked', [{ op: 'addSlide', scene: 'xcluster', slide: slide('s-new'), index: 1 }], (d) => d.scenes.xcluster!.slides, ['s-x-1', 's-new', 's-x-2']],
    ['moves a slide to another scene', [{ op: 'moveSlide', id: 's-x-2', scene: 'recap', index: 0 }], (d) => [d.scenes.xcluster!.slides, d.scenes.recap!.slides], [['s-x-1'], ['s-x-2', 's-recap']]],
    ['adds a choice to a fork', [{ op: 'connect', edge: { id: 'e-new', from: 'geo', to: 'recap', kind: 'choice', label: 'Skip', key: '3' } }], (d) => d.edges.length, 13],
    ['relabels a choice', [{ op: 'updateEdge', id: 'e3', patch: { label: 'AMP' } }], (d) => d.edges.find((e) => e.id === 'e3')!.label, 'AMP'],
    ['adds a scene whose id an object also has (a talk on JavaScript)', [{ op: 'addScene', scene: { id: 'constructor', title: 'Constructors', intent: '', slides: [], detour: true } }], (d) => d.scenes['constructor']?.title, 'Constructors'],
  ])('%s', (_n, ops, read, want) => {
    expect(read(applyOps(sampleDeck(), ops))).toEqual(want);
  });

  it.each<[string, Op[], RegExp]>([
    ['an operation it doesn\'t know', [{ op: 'explode' } as never], /isn't a change Presenter knows/],
    ['a scene id that\'s taken', [{ op: 'addScene', scene: { id: 'amp', title: 'x', intent: '', slides: [] } }], /already a scene "amp"/],
    ['a slide that isn\'t there', [{ op: 'updateSlide', slide: slide('ghost') }], /no slide "ghost"/],
    ['a result that breaks the deck', [{ op: 'disconnect', id: 'e1' }], /can't be reached/],
    ['the second of two when it fails', [{ op: 'setTitle', title: 'New' }, { op: 'removeSlide', id: 'nope' }], /Change 2: there's no slide "nope"/],
    ['an update with no patch', [{ op: 'updateEdge', id: 'e5', label: 'Serverless' } as never], /needs a patch/],
    ['a move of a slide the deck only inherits', [{ op: 'moveSlide', id: 'constructor', scene: 'recap', index: 0 }], /no slide "constructor"/],
  ])('refuses %s, leaving the deck as it was', (_n, ops, why) => {
    const d = sampleDeck();
    const before = JSON.stringify(d);
    const r = tryOps(d, ops);
    expect(r.ok).toBe(false);
    expect(r.ok ? '' : r.problems.join(' ')).toMatch(why);
    expect(JSON.stringify(d)).toBe(before);
  });
});

describe('change sets', () => {
  it('leaves unticked what would overwrite a hand edit, and ticks the rest', () => {
    const d = sampleDeck();
    d.slides['s-amp-1']!.handEdited = true;
    const ops: Op[] = [
      { op: 'updateSlide', slide: slide('s-amp-1') },
      { op: 'updateSlide', slide: slide('s-meko-1') },
      { op: 'removeScene', id: 'amp' },
      { op: 'setTitle', title: 'New' },
    ];
    expect(defaultTicks(d, ops)).toEqual([false, true, false, true]);
    expect(ops.map((o) => describeOp(d, o).sym)).toEqual(['~', '~', '-', '~']);
    expect(describeOp(d, ops[0]!).text).toBe('Slide AMP serverless · 1');
  });

  it('counts a new file under the name of an asset a hand-edited slide shows as a hand edit', () => {
    const d = applyOps(sampleDeck(), [
      { op: 'addAsset', name: 'chart.svg', asset: { hash: 'a'.repeat(64), size: 1, type: 'image/svg+xml' } },
      { op: 'updateSlide', slide: slide('s-amp-1', [{ type: 'image', asset: 'chart.svg', alt: '' }], { handEdited: true }) },
    ]);
    const swap: Op = { op: 'addAsset', name: 'chart.svg', asset: { hash: 'b'.repeat(64), size: 1, type: 'image/svg+xml' } };
    expect([describeOp(d, swap).handEdited, describeOp(d, { ...swap, name: 'other.svg' }).handEdited]).toEqual([true, false]);
  });
});
