import type { DatapackGroups } from '../shared/datapacks.js';
import type { Deck } from '../shared/deck.js';
import type { Op } from '../shared/ops.js';
import type { RunState } from '../shared/run.js';
import type {
  ApiError, AskScope, ChangeSet, CreateDeckRequest, DeckState, DeckSummary, DeckVersion, Health, Job, JobRequest,
} from '../shared/types.js';
import { appUrl } from './base.js';

export class RequestError extends Error {
  constructor(message: string, readonly status: number, readonly problems: string[] = []) {
    super(message);
  }
}

/** A sign-in that ran out sends Meko's sign-in page instead of JSON: back to it, then here. */
function signIn(): never {
  location.href = `/signin?next=${encodeURIComponent(location.pathname + location.search)}`;
  throw new RequestError('Sign in to Meko again.', 401);
}

async function request<T>(method: string, path: string, body?: unknown, init: RequestInit = {}): Promise<T> {
  const res = await fetch(appUrl(path), {
    method,
    credentials: 'same-origin',
    headers: body !== undefined && !(body instanceof Blob) ? { 'Content-Type': 'application/json' } : undefined,
    body: body === undefined ? undefined : body instanceof Blob ? body : JSON.stringify(body),
    ...init,
  });
  if (res.status === 401) signIn();
  const type = res.headers.get('content-type') ?? '';
  if (!type.includes('application/json')) {
    if (res.ok) return undefined as T;
    throw new RequestError(res.status === 502 ? 'Present isn\'t running. Start it with meko start.' : `The request failed (${res.status}).`, res.status);
  }
  const data = (await res.json()) as unknown;
  if (!res.ok) {
    // Present's errors are {error: text}; Meko's own (its proxy's when Present isn't answering) {error: {code, message}}.
    const e = data as Partial<ApiError> | { error?: { message?: unknown }; problems?: undefined };
    const message = typeof e.error === 'string' ? e.error : typeof e.error?.message === 'string' ? e.error.message : `The request failed (${res.status}).`;
    throw new RequestError(message, res.status, Array.isArray(e.problems) ? e.problems : []);
  }
  return data as T;
}

export const api = {
  health: () => request<Health>('GET', '/api/health'),
  datapacks: () => request<DatapackGroups>('GET', '/api/datapacks'),
  decks: () => request<DeckSummary[]>('GET', '/api/decks'),
  createDeck: (r: CreateDeckRequest) => request<DeckState>('POST', '/api/decks', r),
  deck: (id: string) => request<DeckState>('GET', `/api/decks/${id}`),
  ops: (id: string, ops: Op[]) => request<DeckState>('POST', `/api/decks/${id}/ops`, { ops }),
  undo: (id: string) => request<DeckState>('POST', `/api/decks/${id}/undo`, {}),
  redo: (id: string) => request<DeckState>('POST', `/api/decks/${id}/redo`, {}),
  save: (id: string) => request<DeckState>('POST', `/api/decks/${id}/save`, {}),
  versions: (id: string) => request<DeckVersion[]>('GET', `/api/decks/${id}/versions`),
  restore: (id: string, hash: string) => request<DeckState>('POST', `/api/decks/${id}/restore`, { hash }),
  addAsset: (id: string, name: string, file: File) =>
    request<DeckState>('PUT', `/api/decks/${id}/assets/${encodeURIComponent(name)}`, file, {
      headers: { 'Content-Type': 'application/octet-stream', 'X-Asset-Type': file.type || 'application/octet-stream' },
    }),
  jobs: (id: string) => request<Job[]>('GET', `/api/decks/${id}/jobs`),
  startJob: (id: string, r: JobRequest) => request<Job>('POST', `/api/decks/${id}/jobs`, r),
  ask: (id: string, instruction: string, scope: AskScope) => request<Job>('POST', `/api/decks/${id}/jobs`, { kind: 'ask', instruction, scope }),
  cancelJob: (job: string) => request<Job>('POST', `/api/jobs/${job}/cancel`, {}),
  changeSets: (id: string) => request<ChangeSet[]>('GET', `/api/decks/${id}/changesets`),
  applyChangeSet: (cs: string, take: number[]) => request<DeckState>('POST', `/api/changesets/${cs}/apply`, { take }),
  discardChangeSet: (cs: string) => request<ChangeSet>('POST', `/api/changesets/${cs}/discard`, {}),
  freshSession: (id: string) => request<{ ok: true }>('POST', `/api/decks/${id}/session/fresh`, {}),
  startRun: (id: string, variant?: string) => request<RunState>('POST', `/api/decks/${id}/runs`, { variant }),
  run: (run: string) => request<{ run: RunState; deck: Deck }>('GET', `/api/runs/${run}`),
};

/** An asset's URL, for <img> and <video>. */
export const assetUrl = (deckId: string, name: string) => appUrl(`/api/decks/${deckId}/assets/${encodeURIComponent(name)}`);
