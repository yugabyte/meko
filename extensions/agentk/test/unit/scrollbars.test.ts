import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { overlayScrollbars } from '../../src/web/hooks/useMedia.js';
import { sidewaysWheel } from '../../src/web/util.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');
const rule = (sel: string) => STYLES.slice(STYLES.indexOf(`\n${sel} {`)).split('}')[0];

describe('session tabs', () => {
  it('scroll with no scrollbar, which would be drawn over the names', () => {
    expect(rule('.tabs')).toContain('overflow-x: auto;');
    expect(rule('.tabs')).toContain('scrollbar-width: none;');
    expect(rule('.tabs::-webkit-scrollbar')).toContain('display: none;');
  });

  it.each([
    [{ deltaX: 0, deltaY: 100, deltaMode: 0 }, 100],
    [{ deltaX: 0, deltaY: -100, deltaMode: 0 }, -100],
    [{ deltaX: 2, deltaY: 30, deltaMode: 0 }, 30],
    [{ deltaX: 0, deltaY: 3, deltaMode: 1 }, 120],
    [{ deltaX: 0, deltaY: -1, deltaMode: 2 }, -600],
    [{ deltaX: 50, deltaY: 10, deltaMode: 0 }, 0],
    [{ deltaX: -40, deltaY: 0, deltaMode: 0 }, 0],
    [{ deltaX: 0, deltaY: 0, deltaMode: 0 }, 0],
  ])('scroll sideways by a wheel turn %o: %d px', (e, px) => {
    expect(sidewaysWheel(e, 600)).toBe(px);
  });
});

describe('overlay scrollbars', () => {
  it.each([
    [{ offsetHeight: 50, clientHeight: 50 }, true],
    [{ offsetHeight: 50, clientHeight: 35 }, false],
  ])('are told by a probe %o: %s', (probe, overlay) => {
    expect(overlayScrollbars(probe)).toBe(overlay);
  });

  it("leave a diff's last row clear", () => {
    expect(rule('.diff-wrap')).toContain('overflow-x: auto;');
    expect(rule('.overlay-scrollbars .diff-wrap')).toMatch(/padding-bottom: 1\dpx;/);
  });
});
