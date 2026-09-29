import { describe, expect, it } from 'vitest';
import { costOf, priceFor } from '../../src/shared/pricing.js';

describe('API list prices', () => {
  it('finds a model by its id, with a date, context or version suffix', () => {
    expect(priceFor('claude-opus-5-5')).toEqual({ input: 4, output: 20, cacheRead: 0.2 });
    expect(priceFor('claude-opus-5')).toEqual({ input: 5, output: 25, cacheRead: 0.5 }); // not Opus 5.5's
    expect(priceFor('claude-opus-5-5[1m]')).toEqual(priceFor('claude-opus-5-5'));
    expect(priceFor('claude-haiku-4-5-20251001')).toEqual({ input: 1, output: 5, cacheRead: 0.1 });
    expect(priceFor('claude-fable-5-1')?.cacheRead).toBe(0.25);
    expect(priceFor('claude-fable-5')?.cacheRead).toBe(1);
  });

  it('has no price for a model it does not know', () => {
    expect(priceFor('claude-fake-1')).toBeNull();
    expect(priceFor('claude-opus-5-50')).toBeNull();
    expect(priceFor('<synthetic>')).toBeNull();
  });

  it('prices each kind of token at its own rate', () => {
    const p = priceFor('claude-opus-5-5')!;
    // 1M of each: input $4, output $20, cache read $0.20, 5-minute write $5, 1-hour write $8.
    expect(costOf({ input: 1e6, output: 1e6, cacheRead: 1e6, cacheWrite: 2e6, cacheWrite1h: 1e6 }, p)).toBeCloseTo(4 + 20 + 0.2 + 5 + 8, 10);
    // A 1-hour share larger than the writes is capped at them.
    expect(costOf({ input: 0, output: 0, cacheRead: 0, cacheWrite: 1e6, cacheWrite1h: 5e6 }, p)).toBeCloseTo(8, 10);
  });
});
