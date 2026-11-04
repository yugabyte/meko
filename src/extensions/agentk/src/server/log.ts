import { appendFileSync, existsSync, mkdirSync, renameSync, statSync } from 'node:fs';
import { dirname } from 'node:path';

export interface Logger {
  info(msg: string, extra?: unknown): void;
  warn(msg: string, extra?: unknown): void;
  error(msg: string, extra?: unknown): void;
}

function fmt(level: string, msg: string, extra?: unknown): string {
  const tail = extra === undefined ? '' : ' ' + (extra instanceof Error ? extra.stack ?? extra.message : JSON.stringify(extra));
  return `${new Date().toISOString()} ${level} ${msg}${tail}`;
}

/** Plain line logs to stdout/stderr; journald adds its own metadata under systemd. */
export const consoleLogger: Logger = {
  info: (m, e) => console.log(fmt('INFO', m, e)),
  warn: (m, e) => console.warn(fmt('WARN', m, e)),
  error: (m, e) => console.error(fmt('ERROR', m, e)),
};

export const silentLogger: Logger = { info: () => {}, warn: () => {}, error: () => {} };

export function errMsg(e: unknown): string {
  if (e && typeof e === 'object' && 'stderr' in e && typeof (e as { stderr: unknown }).stderr === 'string') {
    const s = ((e as { stderr: string }).stderr || '').trim();
    if (s) return s.split('\n').slice(-3).join(' ');
  }
  return e instanceof Error ? e.message : String(e);
}

/**
 * Also appends every line to a file (rotated at 5 MB, 3 kept). Journald on
 * some hosts keeps no user logs, so this is what `me_koder logs` reads.
 */
export function fileLogger(file: string, inner: Logger = consoleLogger, maxBytes = 5 * 1024 * 1024, keep = 3): Logger {
  mkdirSync(dirname(file), { recursive: true });
  const write = (level: string, msg: string, extra?: unknown) => {
    try {
      if (existsSync(file) && statSync(file).size > maxBytes) {
        for (let i = keep - 1; i >= 1; i--) if (existsSync(`${file}.${i}`)) renameSync(`${file}.${i}`, `${file}.${i + 1}`);
        renameSync(file, `${file}.1`);
      }
      appendFileSync(file, fmt(level, msg, extra) + '\n', { mode: 0o600 }); // may contain a setup link
    } catch {
      /* logging must never take the server down */
    }
  };
  return {
    info: (m, e) => (inner.info(m, e), write('INFO', m, e)),
    warn: (m, e) => (inner.warn(m, e), write('WARN', m, e)),
    error: (m, e) => (inner.error(m, e), write('ERROR', m, e)),
  };
}
