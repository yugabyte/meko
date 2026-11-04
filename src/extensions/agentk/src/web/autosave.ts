// Settings that save as they're changed (Settings › General, Harness, Code review and Work queue): one request at a time,
// each with only the fields of one change, in the order the changes were made.

export interface AutoSaveStatus {
  state: 'idle' | 'saving' | 'saved';
  /**
   * Why a save failed in the current or last run (the requests sent one after another until none
   * were left), or null while every one of them saved.
   */
  error: unknown;
}

export const AUTO_SAVE_IDLE: AutoSaveStatus = { state: 'idle', error: null };

export interface AutoSave<P> {
  /** Saves the patch once the changes before it are answered; resolves as `flush()` does. */
  save(patch: P): Promise<boolean>;
  /** Resolves once every change so far is answered: true when the run saved them all. */
  flush(): Promise<boolean>;
}

export function autoSave<P extends object, R>(
  send: (patch: P) => Promise<R>,
  opts: {
    /** The server's answer to one change, with the changes still waiting to go. */
    onSaved: (answer: R, patch: P, queued: readonly P[]) => void;
    /** A change didn't save. */
    onFailed: (error: unknown, patch: P, queued: readonly P[]) => void;
    onStatus: (status: AutoSaveStatus) => void;
    /** How long Saved shows. */
    savedMs?: number;
  },
): AutoSave<P> {
  const queue: P[] = [];
  let running: Promise<boolean> | null = null;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const run = async (): Promise<boolean> => {
    let failed: { error: unknown } | null = null;
    for (let patch = queue.shift(); patch; patch = queue.shift()) {
      clearTimeout(timer);
      opts.onStatus({ state: 'saving', error: failed?.error ?? null });
      try {
        const answer = await send(patch);
        opts.onSaved(answer, patch, queue.slice());
      } catch (e) {
        failed = { error: e };
        opts.onFailed(e, patch, queue.slice());
      }
    }
    // In the same turn as the last look at the queue, so a save from here on starts a new run.
    running = null;
    if (failed) {
      opts.onStatus({ state: 'idle', error: failed.error });
      return false;
    }
    opts.onStatus({ state: 'saved', error: null });
    timer = setTimeout(() => opts.onStatus(AUTO_SAVE_IDLE), opts.savedMs ?? 2500);
    return true;
  };

  const flush = (): Promise<boolean> => running ?? Promise.resolve(true);

  const save = (patch: P): Promise<boolean> => {
    queue.push(patch);
    running ??= run();
    return running;
  };

  return { save, flush };
}

/**
 * `cur` with the fields a change sent taken from `from` (the server's answer, or its last one when
 * the change failed), except the fields a change still waiting will set: they keep showing it.
 */
export function settle<T extends object>(cur: T, from: T, patch: object, queued: readonly object[]): T {
  const next = { ...cur } as Record<string, unknown>;
  for (const k of Object.keys(patch)) {
    if (!queued.some((q) => k in q)) next[k] = (from as Record<string, unknown>)[k];
  }
  return next as T;
}
