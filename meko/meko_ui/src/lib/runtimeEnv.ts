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
