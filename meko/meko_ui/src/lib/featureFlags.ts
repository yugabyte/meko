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
} as const;

export type FeatureFlagName = keyof typeof FEATURE_FLAGS;
