// Reads runtime feature flags. The Express server serves `/runtime-config.js`
// (see server/index.ts), which sets `window.__ADMIN_RUNTIME__` from the pod's
// env before the app bundle runs. readRuntimeEnv prefers that value and falls
// back to a build-time VITE_-prefixed import.meta.env var for local dev.

declare global {
  interface Window {
    __ADMIN_RUNTIME__?: Record<string, string | undefined>;
  }
}

export function readRuntimeEnv(name: string): string | undefined {
  const runtime = globalThis.window?.__ADMIN_RUNTIME__;
  if (runtime && runtime[name] != null) return runtime[name];
  return (import.meta.env as Record<string, string | undefined>)[`VITE_${name}`];
}
