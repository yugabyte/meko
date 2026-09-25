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
