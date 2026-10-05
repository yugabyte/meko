import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { FEATURE_FLAGS, type FeatureFlagName } from "@/lib/featureFlags";

/**
 * Reads the latch as three states, not two: "true" and "false" are explicit
 * per-browser choices, and `null` means the user has never chosen, so the
 * caller's default applies.
 *
 * The third state is what lets a flag default to ON (from a deployment flag)
 * and still be turned off with `?flag=false` — clearing the key instead would
 * just fall back to the default again on the next render.
 */
function readLatched(storageKey: string): boolean | null {
  try {
    const raw = localStorage.getItem(storageKey);
    if (raw === "true") return true;
    if (raw === "false") return false;
    return null;
  } catch {
    return null;
  }
}

/**
 * Generic URL-driven feature flag hook. Returns the current value of the named
 * flag and side-effects localStorage when the URL carries
 * `?<queryParam>=true|false`. Query-param value takes precedence on the current
 * render (no first-paint flash on deep-link unlock).
 *
 * `defaultValue` is used only when the user has made no choice for this flag in
 * this browser. Pass a deployment-level gate here (see
 * `lib/projectsFeature.ts`) to get "on where the environment enables it, with a
 * per-browser override either way".
 */
export function useFeatureFlag(name: FeatureFlagName, defaultValue = false): boolean {
  const { storageKey, queryParam } = FEATURE_FLAGS[name];
  const location = useLocation();
  const [latched, setLatched] = useState<boolean | null>(() => readLatched(storageKey));

  const params = new URLSearchParams(location.search);
  const queryVal = params.get(queryParam);

  useEffect(() => {
    if (queryVal !== "true" && queryVal !== "false") return;
    const next = queryVal === "true";
    try {
      // Persist "false" rather than removing the key: an explicit opt-out has
      // to outlive the render, even for a flag whose default is on.
      localStorage.setItem(storageKey, String(next));
    } catch {
      // best-effort persistence; the in-memory state still works.
    }
    setLatched(next);
  }, [queryVal, storageKey]);

  if (queryVal === "true") return true;
  if (queryVal === "false") return false;
  return latched ?? defaultValue;
}
