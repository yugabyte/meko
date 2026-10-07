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
  it('pops the percent up over the ring on hover, on a desktop', () => {
    const html = renderToStaticMarkup(createElement(ContextRing, { percent: 80 }));
    expect(html).toContain('<span class="ctx-pct" aria-hidden="true">80%</span>');
    const css = STYLES.slice(STYLES.indexOf('.ctx-pct {'), STYLES.indexOf('@media (prefers-reduced-motion', STYLES.indexOf('.ctx-pct {')));
    // Hidden until hovered, and floated so the footer doesn't shift; a touch screen can't hover.
    expect(css).toMatch(/\.ctx-pct \{[^}]*display: none;[^}]*position: absolute;/);
    expect(css).toMatch(/@media \(hover: hover\) \{\s*\.ctx:hover \.ctx-pct \{\s*display: block;/);
  });

  it('writes the percent after the ring in the phone\'s sheet, with no pop-up', () => {
    const html = renderToStaticMarkup(createElement(ContextRing, { percent: 80, showPercent: true }));
    expect(html).not.toContain('ctx-pct');
    expect(html).toMatch(/<\/svg>80%<\/span>$/);
  });
});
