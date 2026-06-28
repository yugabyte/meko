import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "path";

// AMP UI dev/build config. Mirrors meko_ui's client setup (React + Tailwind v4
// via the official Vite plugin) but without the Replit-specific plugins or the
// Express/proxy server — AMP's UI is a standalone SPA for now.
export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(import.meta.dirname, "src"),
    },
  },
  build: {
    outDir: path.resolve(import.meta.dirname, "dist"),
    emptyOutDir: true,
  },
  server: {
    host: "0.0.0.0",
    port: 3020,
  },
});
