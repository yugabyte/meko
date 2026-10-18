import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ChangedFile } from '../../src/shared/types.js';
import { FileDiff } from '../../src/web/components/ChangesPanel.js';
import { parsePatch } from '../../src/web/diff.js';

const file = { path: 'gen.txt', oldPath: null, kind: 'added', added: 5000, removed: 0, binary: false, uncommitted: false, truncated: false } as ChangedFile;
const lines = parsePatch(['@@ -0,0 +1,4999 @@', ...Array.from({ length: 4999 }, (_, i) => `+line ${i + 1}`)].join('\n'));

/** The rendered diff, with a comment after row 4500. */
const render = (limit: number) =>
  renderToStaticMarkup(createElement(FileDiff, {
    file, lines, limit, onMore: () => {}, picked: null, onComment: () => {},
    imageUrl: () => '',
    renderAfter: (i: number) => (i === 4500 ? createElement('p', null, 'a comment') : null),
  }));

describe('a large diff', () => {
  it.each<[number, number, string]>([
    [0, 0, '<span>Large diff: 4,999 lines.</span><button class="btn small">Show diff</button>'],
    [2000, 2000, '<button class="btn small">Show 2,000 more lines (3,000 left)</button>'],
    [4000, 4000, '<button class="btn small">Show the last 1,000 lines</button>'],
  ])('shows %i rows, then a button for the rest, with comments on the hidden rows under it', (limit, rows, button) => {
    const html = render(limit);
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
