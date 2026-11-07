import { describe, expect, it } from 'vitest';
import { deckDatapackName, groupDatapacks } from '../../src/shared/datapacks.js';
import { assetFits, deckProblems, MAX_ASSET_BYTES, MAX_DECK_BYTES, type Deck } from '../../src/shared/deck.js';
import { sampleDeck, slide } from '../helpers.js';

const HASH = 'a'.repeat(64);

describe('deck rules', () => {
  it('takes the sample deck', () => {
    expect(deckProblems(sampleDeck())).toEqual([]);
  });

  it.each<[string, (d: Deck) => void, RegExp]>([
    ['a start that isn\'t a scene', (d) => (d.start = 'nope'), /start isn't one of its scenes/],
    ['an edge to a scene that isn\'t there', (d) => d.edges.push({ id: 'x', from: 'menu', to: 'gone', kind: 'next' }), /names a scene the deck doesn't have/],
    ['two edges with one id', (d) => d.edges.push({ ...d.edges[0]! }), /Two edges have id/],
    ['a scene no edge reaches', (d) => (d.scenes.lost = { id: 'lost', title: 'Lost', intent: '', slides: [] }), /"Lost" can't be reached/],
    ['a fork with one choice', (d) => (d.edges = d.edges.filter((e) => e.id !== 'e7')), /"Latency or zero loss\?" needs two or more choices/],
    ['a choice from a scene that isn\'t a fork', (d) => d.edges.push({ id: 'x', from: 'ybdb', to: 'amp', kind: 'choice', label: 'A', key: '1' }), /isn't a fork/],
    ['two choices with one key', (d) => (d.edges.find((e) => e.id === 'e3')!.key = '1'), /distinct key/],
    ['two choices with one label', (d) => (d.edges.find((e) => e.id === 'e3')!.label = 'distributed sql'), /distinct label/],
    ['a slide without the first variant', (d) => (d.slides['s-amp-1']!.variants = { deep: { blocks: [] } }), /no "overview" variant/],
    ['a slide in two scenes', (d) => d.scenes.amp!.slides.push('s-meko-1'), /in two scenes/],
    ['a slide in no scene', (d) => (d.slides.orphan = slide('orphan')), /isn't in any scene/],
    ['an image of an asset the deck lacks', (d) => (d.slides['s-amp-1'] = slide('s-amp-1', [{ type: 'image', asset: 'x.png', alt: '' }])), /asset "x.png", which the deck doesn't have/],
    ['an animation without a datapack', (d) => (d.slides['s-amp-1'] = slide('s-amp-1', [{ type: 'animation', ref: 'raft', params: {} }])), /<datapack>:<name>/],
    ['a block type there isn\'t', (d) => (d.slides['s-amp-1'] = slide('s-amp-1', [{ type: 'poll' } as never])), /"poll" isn't a block type/],
    ['an unknown layout', (d) => (d.slides['s-amp-1']!.layout = 'grid' as never), /layout "grid"/],
    ['an asset over 100 MiB', (d) => (d.assets.big = { hash: HASH, size: MAX_ASSET_BYTES + 1, type: 'video/mp4' }), /over the 100 MiB limit/],
    ['an asset named with a slash', (d) => (d.assets['a/b.png'] = { hash: HASH, size: 1, type: 'image/png' }), /needs a name of letters/],
    // A plain object answers for inherited names too: none of them is a slide, scene or asset.
    ['a slide the deck only inherits', (d) => d.scenes.recap!.slides.push('constructor'), /lists slide "constructor", which the deck doesn't have/],
    ['an edge to a scene the deck only inherits', (d) => d.edges.push({ id: 'x', from: 'recap', to: 'toString', kind: 'next' }), /names a scene the deck doesn't have/],
    ['an image of an asset the deck only inherits', (d) => (d.slides['s-amp-1'] = slide('s-amp-1', [{ type: 'image', asset: 'valueOf', alt: '' }])), /asset "valueOf", which the deck doesn't have/],
    // The pages show these as text: an object (say, notes per variant) would stop them.
    ['notes that aren\'t text', (d) => (d.slides['s-amp-1']!.notes = { overview: 'x' } as never), /notes that aren't text/],
    ['a quote by someone that isn\'t text', (d) => (d.slides['s-amp-1'] = slide('s-amp-1', [{ type: 'quote', text: 'Fast', by: { name: 'Jane' } as never }])), /who said it/],
    ['a caption that isn\'t text', (d) => (d.assets.v = { hash: HASH, size: 1, type: 'video/mp4' }, d.slides['s-amp-1'] = slide('s-amp-1', [{ type: 'video', asset: 'v', caption: 5 as never }])), /caption isn't text/],
    ['a source that isn\'t one', (d) => (d.slides['s-amp-1']!.sources = [null as never]), /a source that isn't/],
    ['a scene intent that isn\'t text', (d) => (d.scenes.amp!.intent = ['a'] as never), /intent that isn't text/],
    ['a choice label that isn\'t text', (d) => (d.edges.find((e) => e.id === 'e3')!.label = 7 as never), /label or key that isn't text/],
  ])('refuses %s', (_name, breakIt, why) => {
    const d = sampleDeck();
    breakIt(d);
    expect(deckProblems(d).join('\n')).toMatch(why);
  });

  it('lets a detour stand alone', () => {
    const d = sampleDeck();
    d.edges = d.edges.filter((e) => e.to !== 'pricing');
    expect(deckProblems(d)).toEqual([]);
  });

  it('takes assets up to the limits, and says why not past them', () => {
    const d = sampleDeck();
    expect(assetFits(d, MAX_ASSET_BYTES)).toBeNull();
    expect(assetFits(d, MAX_ASSET_BYTES + 1)).toMatch(/bigger than 100 MiB, the most one asset can be/);
    for (let i = 0; i < 10; i++) d.assets[`v${i}`] = { hash: HASH, size: MAX_ASSET_BYTES, type: 'video/mp4' };
    expect(assetFits(d, 30 * 1024 * 1024)).toMatch(/has 24 MiB of its 1024 MiB left/);
    expect(MAX_DECK_BYTES).toBe(1024 * 1024 * 1024);
  });
});

describe('datapacks', () => {
  it('groups Presenter\'s own by prefix, and every other one is content', () => {
    const g = groupDatapacks([
      { id: '1', name: 'present/template/yb-dark', description: '' },
      { id: '2', name: 'ybdb/xcluster', description: '' },
      { id: '3', name: 'present/anim/ybdb', description: '' },
      { id: '4', name: 'present/deck/acme', description: '' },
      { id: '5', name: 'adlc/proj/agent-k-local/present', description: '' },
    ]);
    expect([g.content, g.templates, g.animations, g.decks].map((l) => l.map((r) => r.id))).toEqual([['5', '2'], ['1'], ['3'], ['4']]);
  });

  it.each([
    ['YugabyteDB for Acme', 'present/deck/yugabytedb-for-acme'],
    ['  ', 'present/deck/deck'],
    ['A'.repeat(80), `present/deck/${'a'.repeat(47)}`],
  ])('names the deck for "%s" within Meko\'s 60 characters', (title, name) => {
    expect(deckDatapackName(title)).toBe(name);
    expect(name.length).toBeLessThanOrEqual(60);
  });
});
