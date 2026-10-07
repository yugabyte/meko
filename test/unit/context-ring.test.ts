import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { ContextRing } from '../../src/web/components/common.js';
import { contextArc, contextLevel } from '../../src/web/util.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

describe('contextLevel', () => {
  it('is ok below 75%', () => {
    expect(contextLevel(0)).toBe('ok');
    expect(contextLevel(74)).toBe('ok');
  });

  it('warns from 75% up to 94%', () => {
    expect(contextLevel(75)).toBe('warn');
    expect(contextLevel(94)).toBe('warn');
  });

  it('is bad from 95%', () => {
    expect(contextLevel(95)).toBe('bad');
    expect(contextLevel(100)).toBe('bad');
    expect(contextLevel(130)).toBe('bad');
  });
});

describe('contextArc', () => {
  it('is the percent, kept to 0–100', () => {
    expect(contextArc(0)).toBe(0);
    expect(contextArc(50)).toBe(50);
    expect(contextArc(100)).toBe(100);
    expect(contextArc(-5)).toBe(0);
    expect(contextArc(130)).toBe(100);
  });
});

describe('ContextRing', () => {
  it('pops the percent up over the ring on hover, and writes it where nothing can hover', () => {
    const html = renderToStaticMarkup(createElement(ContextRing, { percent: 80 }));
    expect(html).toContain('<span class="ctx-pct" aria-hidden="true">80%</span>');
    // Only a device that can hover hides it and floats it, so a touch tablet's footer still shows it.
    const hover = STYLES.slice(STYLES.indexOf('@media (hover: hover) {\n  .ctx-pct {'));
    expect(hover).toMatch(/^@media \(hover: hover\) \{\s*\.ctx-pct \{[^}]*display: none;[^}]*position: absolute;[^}]*\}\s*\.ctx:hover \.ctx-pct \{\s*display: block;\s*\}\s*\}/);
    expect(STYLES.match(/^\s*\.ctx-pct \{/gm)).toHaveLength(1);
  });

  it('sits over the terminal but under the Changes file menu', () => {
    const z = (sel: string) => Number(STYLES.slice(STYLES.indexOf(`${sel} {`)).match(/z-index: (\d+);/)?.[1]);
    expect(z('.ctx-pct')).toBeGreaterThan(z('.term-toast'));
    expect(z('.ctx-pct')).toBeLessThan(z('.files-pop'));
    // xterm's own layers (up to 11) stay inside the terminal.
    expect(STYLES.slice(STYLES.indexOf('.term-host {')).split('}')[0]).toContain('isolation: isolate;');
  });

  it('writes the percent after the ring in the phone\'s sheet, with no pop-up', () => {
    const html = renderToStaticMarkup(createElement(ContextRing, { percent: 80, showPercent: true }));
    expect(html).not.toContain('ctx-pct');
    expect(html).toMatch(/<\/svg>80%<\/span>$/);
  });
});
