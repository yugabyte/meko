import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// In dev, the API server runs on 127.0.0.1:7777 (plain HTTP on loopback).
const api = process.env.AK_DEV_API ?? 'http://127.0.0.1:7777';

export default defineConfig({
  root: 'src/web',
  plugins: [react()],
  // xterm.js alone is ~300 kB; one bundle is fine for an app served from the same host.
  build: { outDir: '../../dist/web', emptyOutDir: true, chunkSizeWarningLimit: 1500 },
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': api,
      '/healthz': api,
      '/ws': { target: api.replace(/^http/, 'ws'), ws: true },
    },
  },
});
