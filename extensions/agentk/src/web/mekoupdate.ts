/**
 * AgentK's updates are Meko's: Meko checks for them, installs them (restarting only AgentK's server,
 * so sessions keep running), rolls back one that keeps failing, and keeps the Update automatically
 * switch. The page reads and drives them through Meko's own routes, root-absolute on Meko's origin
 * (not under BASE), as `signin.ts` does: `GET /api/v1/extensions` (AgentK's row), `POST
 * /api/v1/extensions/agentk/check` and `/update`, and `PATCH /api/v1/extensions/agentk`.
 *
 * One store for the page (`useMekoUpdate`): AgentK's row is read when a view first needs it, every
 * IDLE_POLL_MS after, and every JOB_POLL_MS while Meko runs a job on AgentK. While that job is an
 * update, the events connection retries fast (`setUpdateInstalling`), and once the server answers as
 * the new version the page reloads into it (`reload.ts`). When the row isn't `updatable`, the store
 * says why: Meko's `not_updatable` (a build from source, or an AgentK still in ~/.agent-k-local,
 * which Meko updates once it has moved), else, from a Meko too old to say or none at all (`npm run
 * dev`), that Meko needs updating first.
 */
import { useSyncExternalStore } from 'react';
import type { MekoExtension } from '../shared/types';
import { setUpdateInstalling } from './events';

const ROUTE = '/api/v1/extensions';
const NAME = 'agentk';
export const JOB_POLL_MS = 2000;
export const IDLE_POLL_MS = 10 * 60_000;

/** What an older Meko, or none, means for updates. */
export const UPDATE_MEKO_FIRST = "This Meko doesn't install AgentK's updates. Update Meko first: meko update.";

async function mekoCall<T>(method: string, path: string, body?: unknown): Promise<T> {
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: body !== undefined ? { 'Content-Type': 'application/json' } : undefined,
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
  } catch {
    throw new Error("Can't reach Meko, which installs AgentK's updates.");
  }
  type Envelope = { data?: unknown; error?: { message?: unknown } };
  const json = (await res.json().catch(() => null)) as Envelope | null;
  if (!res.ok) {
    // An older Meko has no such route (and under `npm run dev` there's no Meko at all).
    if (res.status === 404 || res.status === 405) throw new Error(UPDATE_MEKO_FIRST);
    const msg = json?.error?.message;
    throw new Error(typeof msg === 'string' && msg ? msg : `Meko answered ${res.status}.`);
  }
  return json?.data as T;
}

/** Meko's routes for AgentK's updates. */
export const mekoUpdates = {
  /** AgentK's row in Meko's extension list, or null when Meko doesn't list it. */
  async row(): Promise<MekoExtension | null> {
    const list = await mekoCall<unknown>('GET', ROUTE);
    return (Array.isArray(list) ? (list as MekoExtension[]).find((e) => e?.name === NAME) : undefined) ?? null;
  },
  /** Asks GitHub for AgentK's newest release now. */
  check: () => mekoCall<MekoExtension>('POST', `${ROUTE}/${NAME}/check`),
  /** Starts installing the newest release, as Meko's job on AgentK. */
  update: () => mekoCall<MekoExtension>('POST', `${ROUTE}/${NAME}/update`),
  /** Turns Update automatically on or off. */
  setAuto: (on: boolean) => mekoCall<MekoExtension>('PATCH', `${ROUTE}/${NAME}`, { auto_update: on }),
};

/** Meko is installing an update of AgentK (its job, someone's or the automatic one). */
export function installing(row: MekoExtension | null | undefined): boolean {
  return row?.job?.action === 'update' && row.job.state === 'running';
}

/** The last update Meko ran on AgentK failed (rolled back, or never switched): its error. */
export function updateFailed(row: MekoExtension | null | undefined): string | null {
  return row?.job?.action === 'update' && row.job.state === 'failed' ? row.job.error || 'Meko gave no reason.' : null;
}

export interface MekoUpdateState {
  /** AgentK's row on Meko, when Meko could be asked. */
  row: MekoExtension | null;
  /** Why the page can't update AgentK through Meko (unreachable, older, not listing it), or null. */
  error: string | null;
  loaded: boolean;
}

let state: MekoUpdateState = { row: null, error: null, loaded: false };
const subscribers = new Set<() => void>();
let timer: ReturnType<typeof setTimeout> | null = null;
let reading: Promise<void> | null = null;

function publish(next: MekoUpdateState): void {
  state = next;
  setUpdateInstalling(installing(next.row));
  for (const f of subscribers) f();
  schedule();
}

function schedule(): void {
  if (timer) clearTimeout(timer);
  timer = null;
  if (!subscribers.size) return;
  const job = state.row?.job?.state === 'running';
  timer = setTimeout(() => void refreshMekoUpdate(), job ? JOB_POLL_MS : IDLE_POLL_MS);
}

/** Why the page can't update AgentK through this row, or null when it can. */
function rowError(row: MekoExtension | null): string | null {
  if (!row) return "Meko doesn't list AgentK.";
  if (row.updatable) return null;
  return row.not_updatable ? `Meko can't update this AgentK: ${row.not_updatable}` : UPDATE_MEKO_FIRST;
}

/** Reads AgentK's row from Meko again (one read at a time). */
export function refreshMekoUpdate(): Promise<void> {
  reading ??= mekoUpdates
    .row()
    .then(
      (row) => publish({ row, error: rowError(row), loaded: true }),
      (e: unknown) => publish({ row: state.row, error: e instanceof Error ? e.message : String(e), loaded: true }),
    )
    .finally(() => {
      reading = null;
    });
  return reading;
}

/** Takes a row an action answered with, so the page shows a job from the click on. */
export function takeRow(row: MekoExtension | null | undefined): void {
  if (row?.name === NAME) publish({ row, error: rowError(row), loaded: true });
}

function subscribe(f: () => void): () => void {
  subscribers.add(f);
  if (!state.loaded && !reading) void refreshMekoUpdate();
  else if (!timer) schedule();
  return () => {
    subscribers.delete(f);
    if (!subscribers.size && timer) {
      clearTimeout(timer);
      timer = null;
    }
  };
}

/** AgentK's updates as Meko has them, kept current while any view shows them. */
export function useMekoUpdate(): MekoUpdateState {
  return useSyncExternalStore(subscribe, () => state);
}
