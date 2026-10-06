import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { whenSettled } from '../../src/web/util.js';

/** A stand-in for a terminal's connection state and its change listeners. */
function source(state: boolean | null) {
  const listeners = new Set<() => void>();
  return {
    state,
    listeners,
    subscribe(fn: () => void) {
      listeners.add(fn);
      return () => void listeners.delete(fn);
    },
    set(next: boolean | null) {
      this.state = next;
      listeners.forEach((l) => l());
    },
  };
}

describe('waiting for a terminal to connect before pasting image paths', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('answers at once when it is already settled', async () => {
    const s = source(true);
    await expect(whenSettled(() => s.state, (fn) => s.subscribe(fn), 1000)).resolves.toBe(true);
    s.state = false;
    await expect(whenSettled(() => s.state, (fn) => s.subscribe(fn), 1000)).resolves.toBe(false);
    expect(s.listeners.size).toBe(0);
  });

  it('waits through changes that settle nothing, then answers and stops listening', async () => {
    const s = source(null);
    const p = whenSettled(() => s.state, (fn) => s.subscribe(fn), 15_000);
    s.set(null); // still reconnecting
    vi.advanceTimersByTime(5000);
    s.set(true); // the socket opened
    await expect(p).resolves.toBe(true);
    expect(s.listeners.size).toBe(0);
  });

  it('gives up when told to, or when the time runs out', async () => {
    const s = source(null);
    const refused = whenSettled(() => s.state, (fn) => s.subscribe(fn), 15_000);
    s.set(false);
    await expect(refused).resolves.toBe(false);

    s.state = null;
    const late = whenSettled(() => s.state, (fn) => s.subscribe(fn), 15_000);
    vi.advanceTimersByTime(15_000);
    await expect(late).resolves.toBe(false);
    expect(s.listeners.size).toBe(0);
  });
});
