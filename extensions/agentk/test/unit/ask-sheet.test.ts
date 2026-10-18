import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { AskRequest, Session } from '../../src/shared/types.js';
import { AskForm, AskSheet } from '../../src/web/components/AskSheet.js';
import { MOBILE_QUERY } from '../../src/web/hooks/useMedia.js';

const DRAFT = '| Preferred | Legacy          |\n| Format()  | StringPrintf    |';

const ask: AskRequest = {
  id: 'toolu_1',
  questions: [
    {
      header: 'Issue',
      question: 'Create this issue?',
      multiSelect: false,
      options: [
        { label: 'Create it', description: 'File the issue as drafted', preview: DRAFT },
        { label: "Don't create", description: 'Stop without filing', preview: null },
      ],
    },
  ],
};
const session = { id: 's1', asking: ask } as Session;

const form = () => renderToStaticMarkup(createElement(AskForm, { session, ask }));
/** The class of the `<pre>` holding the preview, and whether its toggle shows as pressed. */
const preview = (html: string) => ({
  pre: /<pre class="([^"]*)">/.exec(html)?.[1],
  pressed: /<button[^>]*aria-pressed="(true|false)"[^>]*>Wrap lines<\/button>/.exec(html)?.[1],
});

describe('AskSheet', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('opens as the full-screen ask dialog', () => {
    const html = renderToStaticMarkup(createElement(AskSheet, { session, onClose: () => {} }));
    expect(html).toMatch(/class="dialog ask-dialog" role="dialog"/);
  });

  it('gives each preview a Wrap lines toggle, unwrapped on a desktop', () => {
    const html = form();
    expect(preview(html)).toEqual({ pre: 'ask-preview', pressed: 'false' });
    // Only options with a preview get one.
    expect(html.match(/Wrap lines/g)).toHaveLength(1);
  });

  it('starts previews wrapped on a phone', () => {
    vi.stubGlobal('matchMedia', (q: string) => ({ matches: q === MOBILE_QUERY }));
    expect(preview(form())).toEqual({ pre: 'ask-preview wrap', pressed: 'true' });
  });
});
