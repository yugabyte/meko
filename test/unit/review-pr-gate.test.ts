import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ReviewingPr, Session } from '../../src/shared/types.js';
import { ReviewPrGate } from '../../src/web/components/ReviewPrGate.js';

const render = (updating: boolean) =>
  renderToStaticMarkup(createElement(ReviewPrGate, {
    session: { reviewingPr: { prState: 'OPEN', headSha: 'b', reviewSha: 'a', newCommits: 2, designUnread: false } as ReviewingPr } as Session,
    reason: null,
    blocked: false,
    midTurn: false,
    updating,
    onAct: async () => {},
  }));

describe('ReviewPrGate', () => {
  it('animates Update to latest, and takes no second press, while it runs', () => {
    expect(render(false)).toContain('>Update to latest</button>');
    const html = render(true);
    expect(html).toContain('<button class="gate" disabled="" title="Check out the PR&#x27;s latest commits; AgentK sums up what changed"><span class="busy">Updating');
    expect(html).not.toContain('Update to latest');
  });
});
