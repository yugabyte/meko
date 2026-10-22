import { execFile } from 'node:child_process';
import { homedir } from 'node:os';
import { withLocalBin } from './paths.js';

/** A Meko running on this computer: its MCP URL and the client token its clients send. */
export interface LocalMeko {
  url: string;
  token: string;
}

const TIMEOUT_MS = 5000;

/**
 * The Meko running on this computer, as `meko token --json` reports it (`meko` on PATH or in
 * ~/.local/bin, where Meko's installer links it), or null: no `meko`, no server running, or an
 * answer that isn't what Meko prints.
 */
export function findLocalMeko(env: NodeJS.ProcessEnv = process.env): Promise<LocalMeko | null> {
  return new Promise((resolve) => {
    const PATH = withLocalBin(env.PATH ?? '', env.HOME || homedir());
    execFile('meko', ['token', '--json'], { env: { ...env, PATH }, timeout: TIMEOUT_MS, encoding: 'utf8' }, (err, stdout) => {
      if (err) return resolve(null);
      try {
        const r = JSON.parse(stdout) as { url?: unknown; token?: unknown; running?: unknown };
        const ok = r.running === true && typeof r.url === 'string' && /^https?:\/\//.test(r.url) && typeof r.token === 'string' && !!r.token;
        resolve(ok ? { url: r.url as string, token: r.token as string } : null);
      } catch {
        resolve(null);
      }
    });
  });
}
