// Deep-link → tab decision for DatapackKnowledgeSummaryPage. Extracted
// so the fingerprint/apply-once logic has a direct unit-test surface
// (see knowledgeSummaryTab.test.ts) — the component wraps these in a
// useEffect + useRef to make "apply only when the fingerprint changes"
// behave correctly. Keeping the pure parts separate also lets the
// component's effect stay a tiny useEffect body.

export type DeepLinkSignals = {
  stateTab?: string;
  queryTab?: string | null;
  promotedMemoryId?: string | null;
};

/** Stable string fingerprint of the three deep-link inputs. Changes
 *  only when at least one input changes; a user's own tab click
 *  doesn't touch any of these and therefore doesn't change the
 *  fingerprint — the effect in the component bails out. */
export function deepLinkTabFingerprint(signals: DeepLinkSignals): string {
  return [signals.stateTab ?? "", signals.queryTab ?? "", signals.promotedMemoryId ?? ""].join("|");
}

/** Does the current deep-link point at Promoted memories? */
export function deepLinkWantsPromotedMemories(signals: DeepLinkSignals): boolean {
  return (
    signals.stateTab === "promoted-memories" ||
    signals.queryTab === "promoted-memories" ||
    !!signals.promotedMemoryId
  );
}
