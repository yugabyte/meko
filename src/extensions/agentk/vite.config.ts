import { createHash } from 'node:crypto';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// In dev, the API server listens on its unix socket in the dev home (`npm run dev:server`), as
// AgentK always does: <home>/web.sock, or a short one under the temp dir when that path is over
// 100 bytes (sockPath() in src/server/paths.ts). AK_DEV_SOCK names another socket.
const home = process.env.AK_DEV_HOME ?? join(fileURLToPath(new URL('.', import.meta.url)), '.dev-home');
function devSocket(): string {
  if (process.env.AK_DEV_SOCK) return process.env.AK_DEV_SOCK;
  const preferred = join(home, 'web.sock');
  if (Buffer.byteLength(preferred) <= 100) return preferred;
  const uid = typeof process.getuid === 'function' ? process.getuid() : 0;
  const tag = createHash('sha256').update(home).digest('hex').slice(0, 12);
  return join(tmpdir(), `agent-k-local-${uid}`, `${tag}-web.sock`);
}
// The browser's Host and Origin go through as they are, as Meko's proxy keeps them (host and port
// only fill in the type: the socket is what's dialed).
const api = { target: { socketPath: devSocket(), host: 'localhost', port: 80 } };

export default defineConfig({
  root: 'src/web',
  // Relative, so the built page loads its scripts, styles and public files from wherever it's
  // served: under /ext/agentk/ behind Meko (the <base> AgentK's server stamps into index.html).
  base: './',
  plugins: [react()],
  // xterm.js alone is ~300 kB; one bundle is fine for an app served from the same host.
  build: { outDir: '../../dist/web', emptyOutDir: true, chunkSizeWarningLimit: 1500 },
  server: {
    host: '127.0.0.1',
    port: 5173,
    // Only the API's own paths: a bare '/api' prefix would also catch the UI's src/web/api.ts.
    proxy: {
      '^/api/': api,
      '/healthz': api,
      '^/ws/': { ...api, ws: true },
    },
  },
});
