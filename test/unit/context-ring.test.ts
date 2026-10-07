import { describe, expect, it } from 'vitest';
import { contextArc, contextLevel } from '../../src/web/util.js';

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
