import { defineConfig } from "vitest/config";
import { fileURLToPath } from "node:url";

// Unit-test config for meko_ui. Kept separate from the app's Vite build.
// jsdom gives the handful of tests that touch window/localStorage a DOM;
// the `@` alias mirrors the app's import paths.
export default defineConfig({
  // Match the app's automatic JSX runtime so component tests don't need a
  // manual `import React`.
  esbuild: { jsx: "automatic" },
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("./src", import.meta.url)),
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
