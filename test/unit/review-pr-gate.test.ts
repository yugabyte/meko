import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ReviewingPr, Session } from '../../src/shared/types.js';
import { ReviewPrGate } from '../../src/web/components/ReviewPrGate.js';

const render = (updating: boolean) =>
  renderToStaticMarkup(createElement(ReviewPrGate, {
    session: { reviewingPr: { prState: 'OPEN', headSha: 'b', reviewSha: 'a', newCommits: 2, designUnread: false } as ReviewingPr } as Session,
    reason: null,
    blocked: updating,
    midTurn: false,
    updating,
    onAct: async () => {},
  }));

describe('ReviewPrGate', () => {
  it('animates Update to latest while it fetches the PR', () => {
    expect(render(false)).toContain('>Update to latest</button>');
    const html = render(true);
    expect(html).toContain('<span class="busy">Updating');
    expect(html).not.toContain('Update to latest');
  });
});
