import { execFile } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { promisify } from 'node:util';
import { parse } from 'smol-toml';
import type { Paths } from './paths.js';

// Versions before the session host ran every Claude in tmux, on socket `agentk` (`[tmux] socket`
// in config.toml), in a tmux service. Only Core.start's guard still looks there.

const exec = promisify(execFile);

/** The tmux socket name older versions used. */
export function legacySocket(paths: Paths): string {
  try {
    const cfg = parse(readFileSync(paths.config, 'utf8')) as { tmux?: { socket?: unknown } };
    if (typeof cfg.tmux?.socket === 'string' && cfg.tmux.socket) return cfg.tmux.socket;
  } catch {
    /* no config, or unreadable: the default */
  }
  return 'agentk';
}

/** The names of the tmux sessions on older versions' socket; none without tmux or a server on it. */
export async function legacyTmuxPanes(paths: Paths, bin = 'tmux'): Promise<Set<string>> {
  try {
    const { stdout } = await exec(bin, ['-L', legacySocket(paths), 'list-sessions', '-F', '#{session_name}']);
    return new Set(stdout.split('\n').filter(Boolean));
  } catch {
    return new Set();
  }
}
