import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ChangedFile } from '../../src/shared/types.js';
import { FileDiff, FilePath } from '../../src/web/components/ChangesPanel.js';
import { parsePatch } from '../../src/web/diff.js';
import { LONG_PRESS_MS, RangeDrag, type CommentPick, type Drag } from '../../src/web/hooks/useRangeDrag.js';

const file = { path: 'gen.txt', oldPath: null, kind: 'added', added: 5000, removed: 0, binary: false, uncommitted: false, truncated: false } as ChangedFile;
const lines = parsePatch(['@@ -0,0 +1,4999 @@', ...Array.from({ length: 4999 }, (_, i) => `+line ${i + 1}`)].join('\n'));

/** The rendered diff, with a comment after row 4500. */
const render = (limit: number, page = 2000) =>
  renderToStaticMarkup(createElement(FileDiff, {
    file, lines, limit, page, onMore: () => {}, picked: null, onComment: () => {},
    imageUrl: () => '',
    renderAfter: (i: number) => (i === 4500 ? createElement('p', null, 'a comment') : null),
  }));

describe('a large diff', () => {
  it.each<[number, number, number, string]>([
    [0, 2000, 0, '<span>Large diff: 4,999 lines.</span><button class="btn small">Show diff</button>'],
    [2000, 2000, 2000, '<button class="btn small">Show 2,000 more lines (3,000 left)</button>'],
    [4000, 2000, 4000, '<button class="btn small">Show the last 1,000 lines</button>'],
    [500, 500, 500, '<button class="btn small">Show 500 more lines (4,500 left)</button>'],
  ])('shows %i rows (%i a click), then a button for the rest, with comments on the hidden rows under it', (limit, page, rows, button) => {
    const html = render(limit, page);
    expect(html.match(/<tr /g)?.length ?? 0).toBe(rows);
    expect(html).toContain(`<div class="chg-more">${button}</div><div class="cmt-thread chg-later"><p>a comment</p></div>`);
  });

  it('renders every row, and the comment in place, once all are shown', () => {
    const html = render(5000);
    expect(html.match(/<tr class="dl-/g)).toHaveLength(5000);
    expect(html).not.toContain('chg-more');
    expect(html).toContain('<div class="cmt-thread"><p>a comment</p></div>');
  });
});

describe('a picked range', () => {
  const small = { ...file, path: 'small.txt', added: 2, removed: 1 } as ChangedFile;
  const rows = parsePatch(['@@ -1,2 +1,3 @@', ' a', '-b', '+B', '+C'].join('\n'));
  const html = renderToStaticMarkup(createElement(FileDiff, {
    file: small, lines: rows, limit: rows.length, page: 2000, onMore: () => {}, onComment: () => {},
    picked: { side: 'new', line: 1, endLine: 3 }, imageUrl: () => '', renderAfter: () => null,
  }));

  it("marks the range's rows on its side only, and its last line, and numbers every row for a drag to find", () => {
    expect(html.match(/<tr [^>]*>/g)).toEqual([
      '<tr class="dl-hunk" data-i="0">',
      '<tr class="dl-ctx picked" data-i="1">',
      '<tr class="dl-del" data-i="2">',
      '<tr class="dl-add picked" data-i="3">',
      '<tr class="dl-add picked pick-end" data-i="4">',
    ]);
  });
});

describe('dragging a range', () => {
  // Rows 1-6 are new lines 1-6, one hunk; rows 8-9 are new lines 20-21, another. Each row is 10 px tall.
  const rows = parsePatch(['@@ -1,6 +1,6 @@', ...'abcdef'.split('').map((c) => ` ${c}`), '@@ -20,2 +20,2 @@', ' x', ' y'].join('\n'));
  let shown: (Drag | null)[], picks: [number, CommentPick][], drag: RangeDrag;
  const pointer = (type: string, y: number, x = 0) => window.dispatchEvent(Object.assign(new Event(type), { pointerId: 1, clientX: x, clientY: y }));
  const press = (row: number, pointerType = 'mouse') =>
    drag.start({ button: 0, isPrimary: true, pointerType, pointerId: 1, clientX: 0, clientY: row * 10 + 5 }, row, 0);

  beforeEach(() => {
    vi.useFakeTimers();
    vi.stubGlobal('window', new EventTarget());
    vi.stubGlobal('requestAnimationFrame', () => 0);
    vi.stubGlobal('cancelAnimationFrame', () => {});
    shown = [];
    picks = [];
    drag = new RangeDrag({
      lines: () => rows,
      rowAt: (_x, y) => Math.floor(y / 10),
      scroller: () => null,
      show: (d) => shown.push(d),
      pick: (i, p) => picks.push([i, p]),
    });
  });
  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  it('makes one comment from a mouse drag, on the row it ended on, and swallows the click after it', () => {
    press(2);
    pointer('pointermove', 45);
    expect(shown.at(-1)).toEqual({ side: 'new', anchor: 2, end: 4 });
    pointer('pointerup', 45);
    expect(picks).toEqual([[4, { from: 2 }]]);
    expect([drag.swallow(), drag.swallow()]).toEqual([true, false]);
  });

  it("ends the range at its hunk's edge when let go past it", () => {
    press(5);
    pointer('pointermove', 95);
    pointer('pointerup', 95);
    expect(picks).toEqual([[6, { from: 5 }]]);
  });

  it.each<[string, () => void]>([
    ['a mouse press that stays on its row', () => {
      press(2);
      pointer('pointermove', 27);
      pointer('pointerup', 27);
    }],
    ['a finger that moves before the long-press (a swipe)', () => {
      press(2, 'touch');
      pointer('pointermove', 45);
      vi.advanceTimersByTime(LONG_PRESS_MS);
      pointer('pointerup', 45);
    }],
    ['a quick tap', () => {
      press(2, 'touch');
      pointer('pointerup', 25);
      vi.advanceTimersByTime(LONG_PRESS_MS);
    }],
  ])('leaves %s to the click', (_, steps) => {
    steps();
    expect(picks).toEqual([]);
    expect(drag.holding()).toBe(false);
    expect(drag.swallow()).toBe(false);
  });

  it('drags with a finger once it has held the + for the long-press, and stops it scrolling', () => {
    press(2, 'touch');
    vi.advanceTimersByTime(LONG_PRESS_MS);
    expect(shown.at(-1)).toEqual({ side: 'new', anchor: 2, end: 2 });
    expect(drag.holding()).toBe(true);
    pointer('pointermove', 45);
    pointer('pointerup', 45);
    expect(picks).toEqual([[4, { from: 2 }]]);
    expect(drag.holding()).toBe(false);
  });

  it('ends a drag on Esc without a comment, and swallows the click after it', () => {
    press(2);
    pointer('pointermove', 45);
    window.dispatchEvent(Object.assign(new Event('keydown'), { key: 'Escape' }));
    pointer('pointerup', 45);
    expect(picks).toEqual([]);
    expect(shown.at(-1)).toBeNull();
    expect(drag.swallow()).toBe(true);
  });
});

describe('a file path', () => {
  const path = (p: string, oldPath: string | null = null) =>
    renderToStaticMarkup(createElement(FilePath, { file: { ...file, path: p, oldPath, kind: oldPath ? 'renamed' : 'modified' } as ChangedFile }));
  const text = (html: string) => html.replace(/<[^>]+>/g, '');

  it('breaks only after each folder, keeping each folder and the name whole, the folders apart from the name', () => {
    const html = path('extensions/agentk/src/web/x.ts');
    expect(html).toContain('class="chg-path chg-file-path mono"');
    expect(html).toContain('<span class="chg-dir"><span class="chg-seg">extensions/</span><wbr/><span class="chg-seg">agentk/</span><wbr/><span class="chg-seg">src/</span><wbr/><span class="chg-seg">web/</span><wbr/></span><span class="chg-seg chg-name">x.ts</span>');
    expect(text(html)).toBe('extensions/agentk/src/web/x.ts');
  });

  it('has no folder part for a file at the root', () => {
    const html = path('README.md');
    expect(html).not.toContain('chg-dir');
    expect(html).toContain('<span class="chg-seg chg-name">README.md</span>');
  });

  it('shows both paths of a rename, each split the same way', () => {
    const html = path('b/new.ts', 'a/old.ts');
    expect(html).toContain('title="a/old.ts → b/new.ts"');
    expect(html).toContain(
      '<span class="chg-dir"><span class="chg-seg">a/</span><wbr/></span><span class="chg-seg chg-name">old.ts</span> → <span class="chg-dir"><span class="chg-seg">b/</span><wbr/></span><span class="chg-seg chg-name">new.ts</span>',
    );
    expect(text(html)).toBe('a/old.ts → b/new.ts');
  });
});
