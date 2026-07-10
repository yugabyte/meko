// Ad-campaign attribution capture. Reads UTM params + per-network click ids from
// the landing URL and persists them first-touch in localStorage, so they survive
// the OAuth redirect (Google/LinkedIn/Cognito) and can be attached to the signup
// identify/track that flows to HubSpot.

const PARAMS = [
  "utm_source",
  "utm_medium",
  "utm_campaign",
  "utm_term",
  "utm_content",
  "gclid", // Google / YouTube
  "fbclid", // Meta / Facebook
  "li_fat_id", // LinkedIn
] as const;

const STORAGE_KEY = "meko_attribution";

/** Persist attribution params from the current URL, first-touch (never overwrite
 *  a stored set). No-op when the URL carries none. Call once at app entry. */
export function captureAttribution(): void {
  if (typeof window === "undefined") return;
  // Whole body guarded: runs at bootstrap, and localStorage access can throw
  // (disabled storage / private mode) — never block app mount over attribution.
  try {
    if (window.localStorage.getItem(STORAGE_KEY)) return; // keep the original touch
    const params = new URLSearchParams(window.location.search);
    const found: Record<string, string> = {};
    for (const key of PARAMS) {
      const value = params.get(key);
      if (value) found[key] = value;
    }
    if (Object.keys(found).length === 0) return;
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(found));
  } catch {
    /* ignore storage quota / privacy-mode / disabled-storage errors */
  }
}

/** The stored attribution set (empty object when none was captured). */
export function getAttribution(): Record<string, string> {
  if (typeof window === "undefined") return {};
  try {
    return JSON.parse(window.localStorage.getItem(STORAGE_KEY) ?? "{}");
  } catch {
    return {};
  }
}
