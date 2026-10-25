import { createHash } from 'node:crypto';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';

// In dev, the server listens on its unix socket in the dev home (`npm run dev:server`), as Presenter
// always does: <home>/run/web.sock, or a short one under /tmp when that path is over 100 bytes
// (sockPath() in src/server/paths.ts). PRESENTER_DEV_SOCK names another socket.
const home = process.env.PRESENTER_DEV_HOME ?? join(fileURLToPath(new URL('.', import.meta.url)), '.dev-home');
function devSocket(): string {
  if (process.env.PRESENTER_DEV_SOCK) return process.env.PRESENTER_DEV_SOCK;
  const preferred = join(home, 'run', 'web.sock');
  if (Buffer.byteLength(preferred) <= 100) return preferred;
  const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
  const tag = createHash('sha256').update(home).digest('hex').slice(0, 12);
  return join('/tmp', `presenter-${uid}`, `${tag}-web.sock`);
}
// The browser's Host and Origin go through as they are, as Meko's proxy keeps them.
const api = { target: { socketPath: devSocket(), host: 'localhost', port: 80 } };

export default defineConfig({
  root: 'src/web',
  // Relative, so the built page loads its files from wherever it's served: under /ext/presenter/
  // behind Meko (the <base> Presenter's server stamps into index.html).
  base: './',
  plugins: [react(), tailwindcss()],
  // Mermaid and React Flow are big; they load only where a page needs them.
  build: { outDir: '../../dist/web', emptyOutDir: true, chunkSizeWarningLimit: 1500 },
  server: {
    host: '127.0.0.1',
    port: 5179,
    proxy: {
      '^/api/': api,
      '^/ws/': { ...api, ws: true },
    },
  },
});
