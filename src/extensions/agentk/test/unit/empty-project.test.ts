import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { Session } from '../../src/shared/types.js';
import { EmptyProject } from '../../src/web/components/EmptyProject.js';

const render = (sessions: Partial<Session>[]) =>
  renderToStaticMarkup(createElement(EmptyProject, {
    project: { id: 'p1', name: 'demo', sessions: sessions as Session[] },
    onNewSession: () => {},
    onClosed: () => {},
    onError: () => {},
  }));

describe('EmptyProject', () => {
  it('offers the red Close project beside New session', () => {
    const html = render([{ id: 's1', status: 'archived', desired: 'archived' }]);
    expect(html).toContain('<div class="empty-actions"><button class="btn danger">Close project</button><button class="btn primary">New session</button></div>');
  });

  it('offers no Close project while the project has an open session, since it would stop it unasked', () => {
    const html = render([{ id: 's1', status: 'working', desired: 'running' }]);
    expect(html).toContain('<div class="empty-actions"><button class="btn primary">New session</button></div>');
  });
});
