/**
 * Anthropic API list prices in USD per million tokens (first-party rates, checked 2026-09-29).
 * Cache writes cost 1.25× the input price for the 5-minute cache and 2× for the 1-hour cache.
 */
export interface ModelPrice {
  input: number;
  output: number;
  cacheRead: number;
}

/** Longest id first, so `claude-opus-5-5` isn't taken for `claude-opus-5`. */
const PRICES: [string, ModelPrice][] = (
  [
    ['claude-fable-5-1', { input: 10, output: 50, cacheRead: 0.25 }],
    ['claude-mythos-5-1', { input: 10, output: 50, cacheRead: 0.25 }],
    ['claude-fable-5', { input: 10, output: 50, cacheRead: 1 }],
    ['claude-mythos-5', { input: 10, output: 50, cacheRead: 1 }],
    ['claude-opus-5-5', { input: 4, output: 20, cacheRead: 0.2 }],
    ['claude-opus-5', { input: 5, output: 25, cacheRead: 0.5 }],
    ['claude-opus-4-8', { input: 5, output: 25, cacheRead: 0.5 }],
    ['claude-opus-4-7', { input: 5, output: 25, cacheRead: 0.5 }],
    ['claude-opus-4-6', { input: 5, output: 25, cacheRead: 0.5 }],
    ['claude-opus-4-5', { input: 5, output: 25, cacheRead: 0.5 }],
    ['claude-opus-4-1', { input: 15, output: 75, cacheRead: 1.5 }],
    ['claude-opus-4-0', { input: 15, output: 75, cacheRead: 1.5 }],
    ['claude-opus-4', { input: 15, output: 75, cacheRead: 1.5 }],
    ['claude-sonnet-5-5', { input: 2, output: 10, cacheRead: 0.2 }],
    ['claude-sonnet-5', { input: 2, output: 10, cacheRead: 0.2 }],
    ['claude-sonnet-4-6', { input: 3, output: 15, cacheRead: 0.3 }],
    ['claude-sonnet-4-5', { input: 3, output: 15, cacheRead: 0.3 }],
    ['claude-sonnet-4-0', { input: 3, output: 15, cacheRead: 0.3 }],
    ['claude-sonnet-4', { input: 3, output: 15, cacheRead: 0.3 }],
    ['claude-haiku-4-5', { input: 1, output: 5, cacheRead: 0.1 }],
    ['claude-3-5-haiku', { input: 0.8, output: 4, cacheRead: 0.08 }],
  ] as [string, ModelPrice][]
).sort((a, b) => b[0].length - a[0].length);

/**
 * The price for a model id as transcripts report it: the bare id, or with a date (`-20251001`), a
 * context suffix (`[1m]`) or a Vertex version (`@…`), and Bedrock's form
 * (`us.anthropic.claude-…-v1:0`). Null for a model with no known price.
 */
export function priceFor(model: string): ModelPrice | null {
  model = model.replace(/^(?:[a-z]+\.)?anthropic\./, '').replace(/-v\d+(?::\d+)?$/, '');
  for (const [id, price] of PRICES) {
    if (model === id || (model.startsWith(id) && /^(?:-\d{8}|\[|@)/.test(model.slice(id.length)))) return price;
  }
  return null;
}

export interface PricedTokens {
  input: number;
  output: number;
  cacheRead: number;
  /** All cache writes, 5-minute and 1-hour. */
  cacheWrite: number;
  /** The 1-hour share of `cacheWrite`. */
  cacheWrite1h: number;
}

/** USD for these tokens at `price`. */
export function costOf(t: PricedTokens, price: ModelPrice): number {
  const write1h = Math.min(t.cacheWrite1h, t.cacheWrite);
  const write5m = t.cacheWrite - write1h;
  return (
    t.input * price.input +
    t.output * price.output +
    t.cacheRead * price.cacheRead +
    write5m * price.input * 1.25 +
    write1h * price.input * 2
  ) / 1e6;
}
