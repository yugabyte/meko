// Central registry of pre-GA UI feature flags.
//
// Each entry pairs a localStorage key (where the flag is latched) with
// the URL query param that toggles it. Visiting any URL with
// `?<queryParam>=true` enables the flag; `?<queryParam>=false` clears it.
//
// To add a new flag: append an entry below, then call useFeatureFlag
// (or read FEATURE_FLAGS directly) wherever you gate on it.

export const FEATURE_FLAGS = {
  /** Plans & billing page + sidebar entry. Latched once unlocked. */
  showPaymentOptions: {
    storageKey: "meko:show_billing_nav",
    queryParam: "showPaymentOptions",
  },
  /**
   * Collections & folders: the `/collections` tree and the collection/path additions to
   * the Datapacks page. Unlike the flag above this one has no default of its
   * own — unset, it follows the `MEKO_ENABLE_COLLECTIONS` deployment flag, and
   * `?collections=true|false` overrides that for one browser. See
   * `lib/collectionsFeature.ts`.
   */
  collections: {
    storageKey: "meko:show_collections",
    queryParam: "collections",
  },
  /**
   * Observe nav entry (the Activity-icon link under the Datapacks list). Hidden by
   * default — most users should not see it yet. Visit `?observe=true` to latch it on,
   * `?observe=false` to clear. The `/observe-hub` route itself still mounts so a
   * direct URL navigation keeps working for internal review.
   */
  observeNav: {
    storageKey: "meko:show_observe_nav",
    queryParam: "observe",
  },
} as const;

export type FeatureFlagName = keyof typeof FEATURE_FLAGS;
