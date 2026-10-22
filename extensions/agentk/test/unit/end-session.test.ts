import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { Session } from '../../src/shared/types.js';
import { EndSessionButton } from '../../src/web/components/SessionSheet.js';

const render = (session: Partial<Session>) =>
  renderToStaticMarkup(createElement(EndSessionButton, {
    session: { id: 's1', branch: 'ak/x', discarding: false, ...session } as Session,
    onAct: async () => {},
    learnTo: 'team/demo',
  }));

describe('EndSessionButton', () => {
  it('opens on Archive', () => {
    expect(render({})).toBe('<button class="btn small">Archive</button>');
  });

  it('says a Discard waits for the learn step, and offers to discard now', () => {
    const html = render({ discarding: true });
    expect(html).toContain('Saving learnings, then discarding');
    expect(html).toMatch(/<button class="btn danger">Discard now<\/button>/);
    expect(html).not.toContain('>Archive<');
  });

  it("says where the learn step is that a Discard waits on, and for how long", () => {
    const html = render({ discarding: true, learnProgress: { step: 'check', facts: 3, done: 0, startedAt: Date.now() - 65_000 } });
    expect(html).toContain('Checking 3 facts against Meko, then discarding');
    expect(html).toMatch(/<span class="elapsed" aria-hidden="true">1m 0[56]s<\/span>/);
  });
});
