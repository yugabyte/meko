import { useEffect, useState } from "react";
import { useLocation } from "react-router-dom";
import { FEATURE_FLAGS, type FeatureFlagName } from "@/lib/featureFlags";

function readLatched(storageKey: string): boolean {
  try {
    return localStorage.getItem(storageKey) === "true";
  } catch {
    return false;
  }
}

/**
 * Generic URL-driven feature flag hook. Returns the current value of
 * the named flag and side-effects localStorage when the URL carries
 * `?<queryParam>=true|false`. Query-param value takes precedence on
 * the current render (no first-paint flash on deep-link unlock).
 */
export function useFeatureFlag(name: FeatureFlagName): boolean {
  const { storageKey, queryParam } = FEATURE_FLAGS[name];
  const location = useLocation();
  const [enabled, setEnabled] = useState<boolean>(() => readLatched(storageKey));

  const params = new URLSearchParams(location.search);
  const queryVal = params.get(queryParam);

  useEffect(() => {
    if (queryVal === "true") {
      try {
        localStorage.setItem(storageKey, "true");
      } catch {
        // best-effort persistence; the in-memory state still works.
      }
      setEnabled(true);
    } else if (queryVal === "false") {
      try {
        localStorage.removeItem(storageKey);
      } catch {
        // best-effort persistence; the in-memory state still works.
      }
      setEnabled(false);
    }
  }, [queryVal, storageKey]);

  if (queryVal === "true") return true;
  if (queryVal === "false") return false;
  return enabled;
}
