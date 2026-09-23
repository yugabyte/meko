// Reads configuration that can be set per-deployment at RUNTIME rather than
// baked into the bundle at build time.
//
// The Express server serves `/runtime-config.js` (see server/index.ts), which
// sets `window.__MEKO_RUNTIME__` from the pod's env before the app bundle runs.
// `readRuntimeEnv` prefers that value and falls back to build-time
// `import.meta.env` (so local dev / `vite build` without the server still work).

declare global {
  interface Window {
    __MEKO_RUNTIME__?: Record<string, string | undefined>;
  }
}

export function readRuntimeEnv(name: string): string | undefined {
  // If the key is present in the runtime config, it wins — even when set to ""
  // ("" is the server emitting the pod's empty env), so operators can disable a
  // build-time-configured var at runtime. Only fall back when the key is absent.
  const runtime = globalThis.window?.__MEKO_RUNTIME__;
  if (runtime && name in runtime) return runtime[name];
  return (import.meta.env as Record<string, string | undefined>)[name];
}

/** Spellings the BE and MCP server also accept for a boolean env var, so one
 *  value means the same thing in every service's ConfigMap. */
const TRUTHY_RUNTIME_VALUES = ["1", "true", "yes"];

/**
 * Reads a runtime env var as an on/off gate. Absent or unrecognised reads as
 * OFF, so a deployment that has not set the var never inherits the feature.
 *
 * Use this for any new deployment-level flag instead of re-deriving the
 * parse — `lib/labs.ts` predates it and spells the same check by hand.
 */
export function readRuntimeFlag(name: string): boolean {
  return TRUTHY_RUNTIME_VALUES.includes((readRuntimeEnv(name) ?? "").trim().toLowerCase());
}
