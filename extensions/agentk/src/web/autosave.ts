// Settings that save as they're changed (Settings › General and Harness): one request at a time,
// each with only the fields that changed, and the changes made while one is out merged into the next.

export interface AutoSaveStatus {
  state: 'idle' | 'saving' | 'saved';
  /** Why the last save failed, until the next one starts. */
  error: unknown;
}

export const AUTO_SAVE_IDLE: AutoSaveStatus = { state: 'idle', error: null };

export interface AutoSave<P> {
  /** Saves the patch, after the request that's out and together with what waits behind it. */
  save(patch: P): Promise<void>;
  /** Resolves once everything saved so far is answered. */
  flush(): Promise<void>;
}

export function autoSave<P extends object, R>(
  send: (patch: P) => Promise<R>,
  opts: {
    /** The server's answer to one request, with the patch it sent. */
    onSaved: (answer: R, patch: P) => void;
    /** A request failed: what it sent didn't save. */
    onFailed: (error: unknown, patch: P) => void;
    onStatus: (status: AutoSaveStatus) => void;
    /** How long Saved shows. */
    savedMs?: number;
  },
): AutoSave<P> {
  let pending: P | null = null;
  let running: Promise<void> | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const run = async (): Promise<void> => {
    let failed: { error: unknown } | null = null;
    while (pending) {
      const patch = pending;
      pending = null;
      clearTimeout(timer);
      opts.onStatus({ state: 'saving', error: null });
      try {
        opts.onSaved(await send(patch), patch);
      } catch (e) {
        failed = { error: e };
        opts.onFailed(e, patch);
      }
    }
    // In the same turn as the last look at `pending`, so a save from here on starts a new run.
    running = null;
    if (failed) return opts.onStatus({ state: 'idle', error: failed.error });
    opts.onStatus({ state: 'saved', error: null });
    timer = setTimeout(() => opts.onStatus(AUTO_SAVE_IDLE), opts.savedMs ?? 2500);
  };

  const flush = (): Promise<void> => running ?? Promise.resolve();

  const save = (patch: P): Promise<void> => {
    pending = { ...pending, ...patch } as P;
    running ??= run();
    return running;
  };

  return { save, flush };
}
