// Unit tests for the deep-link → tab decision used by
// DatapackKnowledgeSummaryPage. Simulates the fingerprint-based
// "apply only when the deep-link changes" effect so the tab-revert
// bug AgentK flagged on MEKO-737 is covered end-to-end.
import { describe, expect, it } from "vitest";
import {
  deepLinkTabFingerprint,
  deepLinkWantsPromotedMemories,
  type DeepLinkSignals,
} from "./knowledgeSummaryTab";

/**
 * Mirrors the component's useEffect body: tracks the last-applied
 * fingerprint and returns the new active tab after one render pass.
 * `onApply` is called with the chosen tab only when the fingerprint
 * actually changes — same guard the real component uses to stop a
 * stale deep-link from bouncing a user tab click back.
 */
function simulate(
  signals: DeepLinkSignals,
  priorActiveTab: string,
  lastFingerprint: { current: string | null },
): { activeTab: string; fingerprint: string | null; applied: boolean } {
  const fingerprint = deepLinkTabFingerprint(signals);
  if (fingerprint === lastFingerprint.current) {
    return { activeTab: priorActiveTab, fingerprint: lastFingerprint.current, applied: false };
  }
  lastFingerprint.current = fingerprint;
  if (deepLinkWantsPromotedMemories(signals)) {
    return { activeTab: "promoted-memories", fingerprint, applied: true };
  }
  return { activeTab: priorActiveTab, fingerprint, applied: true };
}

describe("knowledgeSummaryTab deep-link decision", () => {
  it("promotes to Promoted memories when the query tab says so", () => {
    const ref: { current: string | null } = { current: null };
    const r = simulate({ queryTab: "promoted-memories" }, "files", ref);
    expect(r.activeTab).toBe("promoted-memories");
    expect(r.applied).toBe(true);
  });

  it("promotes to Promoted memories when promotedMemoryId is set", () => {
    const ref: { current: string | null } = { current: null };
    const r = simulate({ promotedMemoryId: "mem_123" }, "files", ref);
    expect(r.activeTab).toBe("promoted-memories");
  });

  it("does NOT bounce a user's Files click back when the deep-link is unchanged", () => {
    // Arrive via ?tab=promoted-memories.
    const ref: { current: string | null } = { current: null };
    simulate({ queryTab: "promoted-memories" }, "files", ref);
    // User clicks Files. Same deep-link signals (query still has tab=
    // because the component doesn't strip the ?tab param). Effect
    // re-runs with the user's new activeTab.
    const r = simulate({ queryTab: "promoted-memories" }, "files", ref);
    expect(r.applied).toBe(false);
    expect(r.activeTab).toBe("files");
  });

  it("re-promotes when a new deep-link signal arrives (TraceDrawer link)", () => {
    const ref: { current: string | null } = { current: null };
    // User lands cold on files.
    simulate({}, "files", ref);
    // TraceDrawer link sets ?promotedMemoryId=…
    const r = simulate({ promotedMemoryId: "mem_abc" }, "files", ref);
    expect(r.applied).toBe(true);
    expect(r.activeTab).toBe("promoted-memories");
  });

  it("stays on the user's chosen tab when the strip-params effect clears promotedMemoryId", () => {
    // Promoted memories arrived via promotedMemoryId.
    const ref: { current: string | null } = { current: null };
    simulate({ promotedMemoryId: "mem_abc" }, "files", ref);
    // Sibling effect strips the param — fingerprint changes, no deep-
    // link now wants Promoted memories, user's current tab stays.
    const r = simulate({}, "promoted-memories", ref);
    expect(r.applied).toBe(true);
    expect(r.activeTab).toBe("promoted-memories");
  });

  it("state.tab and query.tab contribute independently to the fingerprint", () => {
    expect(deepLinkTabFingerprint({ stateTab: "promoted-memories" })).not.toBe(
      deepLinkTabFingerprint({ queryTab: "promoted-memories" }),
    );
  });
});
