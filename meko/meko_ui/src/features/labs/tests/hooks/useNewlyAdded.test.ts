// The flash says "this row is what the lesson you just finished produced".
// Anything that fires it on rows the user did not just create says the
// opposite, which is what a page refresh used to do.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";

import { useNewlyAdded } from "@/features/labs/hooks/useNewlyAdded";

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("useNewlyAdded", () => {
  // The bug: the feed starts EMPTY and fills in after its first fetch. A
  // baseline taken from that first render recorded nothing, so every row
  // looked new the moment the data landed, on every refresh.
  it("takes no baseline until the feed has loaded", () => {
    const { result, rerender } = renderHook(({ ids, ready }) => useNewlyAdded(ids, ready), {
      initialProps: { ids: [] as string[], ready: false },
    });

    // The fetch resolves: rows appear, but they were already there.
    rerender({ ids: ["a", "b"], ready: true });
    expect(result.current.size).toBe(0);
  });

  it("flashes only what appeared after that baseline", () => {
    const { result, rerender } = renderHook(({ ids, ready }) => useNewlyAdded(ids, ready), {
      initialProps: { ids: [] as string[], ready: false },
    });
    rerender({ ids: ["a"], ready: true });

    rerender({ ids: ["a", "b"], ready: true });
    expect([...result.current]).toEqual(["b"]);
  });

  it("stops flashing after the highlight window", () => {
    const { result, rerender } = renderHook(({ ids, ready }) => useNewlyAdded(ids, ready), {
      initialProps: { ids: [] as string[], ready: false },
    });
    rerender({ ids: ["a"], ready: true });
    rerender({ ids: ["a", "b"], ready: true });
    expect(result.current.size).toBe(1);

    act(() => {
      vi.advanceTimersByTime(5_000);
    });
    expect(result.current.size).toBe(0);
  });

  // Switching datapack sends the feed back to EMPTY. Keeping the old
  // baseline would light up the whole of the next datapack.
  it("drops its baseline when the feed goes unloaded again", () => {
    const { result, rerender } = renderHook(({ ids, ready }) => useNewlyAdded(ids, ready), {
      initialProps: { ids: [] as string[], ready: false },
    });
    rerender({ ids: ["a"], ready: true });

    rerender({ ids: [], ready: false });
    rerender({ ids: ["x", "y"], ready: true });
    expect(result.current.size).toBe(0);
  });
});
