import { defineConfig, type UserConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";
import runtimeErrorOverlay from "@replit/vite-plugin-runtime-error-modal";

export default defineConfig(async ({ command }): Promise<UserConfig> => ({
  plugins: [
    react(),
    runtimeErrorOverlay(),
    tailwindcss(),
    ...(process.env.NODE_ENV !== "production" &&
    process.env.REPL_ID !== undefined
      ? [
          await import("@replit/vite-plugin-cartographer").then((m) =>
            m.cartographer(),
          ),
          await import("@replit/vite-plugin-dev-banner").then((m) =>
            m.devBanner(),
          ),
        ]
      : []),
  ],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "src"),
      "@shared": path.resolve(import.meta.dirname, "shared"),
      "@assets": path.resolve(import.meta.dirname, "attached_assets"),
    },
  },
  css: {
    postcss: {
      plugins: [],
    },
  },
  root: path.resolve(import.meta.dirname),
  // Expose MEKO_FREE_TIER_* / MEKO_STANDARD_TIER_* env vars (in addition to the
  // default VITE_* ones) so api_server's `free_tier.go` and `src/lib/freeTier.ts`
  // can share a single env-var name per cap. This covers the Vite-only dev path;
  // production reads the same names via the server's RUNTIME_ENV_KEYS. See
  // free_tier.go for the names.
  //
  // MEKO_ENABLE_* is added on `serve` ONLY, so the deployment feature flags can
  // be set from .env for local dev. It must never be inlined by `vite build`:
  // readRuntimeEnv falls back to import.meta.env for any key the pod does not
  // serve, so a baked-in value would turn a flag on in a deployment that never
  // set it -- breaking the "off unless explicitly enabled" promise in
  // lib/labs.ts and lib/byoc.ts. The Dockerfile's `COPY . .` would carry a
  // developer's .env into the build, which is why this is gated here rather
  // than trusted to .dockerignore alone.
  envPrefix: [
    "VITE_",
    "MEKO_FREE_TIER_",
    "MEKO_STANDARD_TIER_",
    "MEKO_PRO_TIER_",
    ...(command === "serve" ? ["MEKO_ENABLE_"] : []),
  ],
  build: {
    outDir: path.resolve(import.meta.dirname, "dist/public"),
    emptyOutDir: true,
  },
  server: {
    host: "0.0.0.0",
    allowedHosts: true,
    fs: {
      strict: true,
      deny: ["**/.*"],
    },
  },
}));
