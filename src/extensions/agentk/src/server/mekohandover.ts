import { spawn } from 'node:child_process';
import { homedir } from 'node:os';
import { withLocalBin } from './paths.js';

/** A Meko Cloud link as AgentK held it before Meko owned it. */
export interface OwnMekoLink {
  url: string;
  apiKey: string;
  /** '' when AgentK had none of its own: Meko works it out. */
  webUrl: string;
  apiUrl: string;
}

/** Meko checks the key (10 s) and a typed API URL (10 s), through its server when it runs. */
const TIMEOUT_MS = 40000;

/**
 * Hands AgentK's own Meko Cloud key to Meko, which owns the link now: `meko cloud_link` with the
 * URLs as arguments and the key on stdin (never on a command line), asked of the Meko in `mekoHome`
 * as `probeLocalMeko` asks. Meko checks the key before it saves it; the error is Meko's own.
 */
export function handOverMekoKey(
  mekoHome: string | null,
  link: OwnMekoLink,
  env: NodeJS.ProcessEnv = process.env,
): Promise<{ ok: true } | { ok: false; error: string }> {
  const args = ['cloud_link', '--url', link.url];
  if (link.webUrl) args.push('--web-url', link.webUrl);
  if (link.apiUrl) args.push('--api-url', link.apiUrl);
  const PATH = withLocalBin(env.PATH ?? '', env.HOME || homedir());
  return new Promise((resolve) => {
    const child = spawn('meko', args, {
      env: { ...env, PATH, ...(mekoHome ? { MEKO_HOME: mekoHome } : {}) },
      stdio: ['pipe', 'ignore', 'pipe'],
      timeout: TIMEOUT_MS,
    });
    let stderr = '';
    child.stderr.setEncoding('utf8').on('data', (d: string) => (stderr = (stderr + d).slice(-2000)));
    child.on('error', (e) => resolve({ ok: false, error: e.message }));
    child.on('close', (code, signal) => {
      if (code === 0) return resolve({ ok: true });
      const why = stderr.trim().split('\n').pop()?.replace(/^meko: /, '') || (signal ? `meko was stopped (${signal})` : `meko exited with ${code}`);
      resolve({ ok: false, error: why });
    });
    child.stdin.on('error', () => undefined);
    child.stdin.end(`${link.apiKey}\n`);
  });
}
