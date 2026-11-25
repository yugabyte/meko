import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { StatusDot } from '../../src/web/components/common.js';
import { TopBar } from '../../src/web/components/TopBar.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

describe('TopBar', () => {
  it('puts the status dot beside the name, outside the box that clips the name', () => {
    const html = renderToStaticMarkup(
      createElement(TopBar, {
        onMenu: () => {},
        sub: 'Project',
        title: 'Fix the logo',
        icon: createElement(StatusDot, { status: 'needs_input', phase: 's1' }),
      }),
    );
    expect(html).toMatch(/<span class="topbar-name"><i class="dot needs"[^>]*><\/i><b>Fix the logo<\/b><\/span>/);
    // The dot's halo and the diamond's corners paint outside its box, so nothing around it may clip.
    const rule = (sel: string) => STYLES.slice(STYLES.indexOf(`\n${sel} {`)).split('}')[0];
    expect(rule('.topbar-title b')).toContain('overflow: hidden;');
    expect(rule('.topbar-name')).not.toContain('overflow');
    expect(rule('.topbar-title')).not.toContain('overflow');
  });
});
