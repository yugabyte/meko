import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import { ContextRing } from '../../src/web/components/common.js';
import { LimitRing } from '../../src/web/components/Rail.js';
import { contextArc, contextLevel } from '../../src/web/util.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

describe('contextLevel', () => {
  // ok below 75%, warn from 75% up to 94%, bad from 95%.
  it.each<[number, string]>([[0, 'ok'], [74, 'ok'], [75, 'warn'], [94, 'warn'], [95, 'bad'], [130, 'bad']])('is %s%% → %s', (percent, level) => {
    expect(contextLevel(percent)).toBe(level);
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

describe('LimitRing', () => {
  // The percent in the ring as it is, the meter and the arc kept to 100, amber from 80% and red from 95%.
  it.each<[number, string | null, number]>([[42, null, 42], [79, null, 79], [80, 'warn', 80], [95, 'bad', 95], [130, 'bad', 100]])('at %s%% is %s, its meter at %s', (pct, level, value) => {
    const html = renderToStaticMarkup(createElement(LimitRing, { label: '5-hour', name: 'Claude 5-hour', pct, title: '', children: 'Fri 9:00 AM' }));
    expect(html).toMatch(new RegExp(`^<div class="lim${level ? ` ${level}` : ''}"`));
    expect(html).toContain(`aria-label="Claude 5-hour used" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${value}"`);
    expect(html).toContain(`>${pct}</text>`);
    expect(html.match(/stroke-dasharray="([\d.]+) 100"/)?.[1]).toBe(value < 100 ? String(value) : undefined);
    expect(html.includes('▲')).toBe(level !== null);
  });
});
