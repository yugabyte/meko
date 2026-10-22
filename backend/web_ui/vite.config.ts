// Builds the web UI into dist/, which meko serve serves at /. npm run dev proxies the API to a
// running Meko (MEKO_URL, default http://127.0.0.1:7778).
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

const target = process.env.MEKO_URL ?? "http://127.0.0.1:7778";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: { outDir: "dist", emptyOutDir: true },
  server: {
    port: 5178,
    // Meko takes only requests addressed to it (Host) and, from a browser, only its own origin, so
    // the proxy sends both as Meko's instead of the dev server's.
    proxy: {
      "/api": { target, changeOrigin: true, headers: { origin: target } },
      "/mcp": { target, changeOrigin: true, headers: { origin: target } },
      "/healthz": { target, changeOrigin: true },
    },
  },
});
