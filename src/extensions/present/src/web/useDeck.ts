import { useCallback, useEffect, useState } from 'react';
import type { Op } from '../shared/ops.js';
import type { ChangeSet, DeckState, Job } from '../shared/types.js';
import { api } from './api.js';
import { useEvents } from './events.js';

export interface DeckHandle {
  state: DeckState | null;
  jobs: Job[];
  changeSets: ChangeSet[];
  error: string | null;
  setError: (e: string | null) => void;
  /** Sends hand edits; the reply (and the event every other page gets) carries the new deck. */
  edit: (ops: Op[]) => Promise<boolean>;
  /** Runs an action, showing its error. */
  act: <T>(f: () => Promise<T>) => Promise<T | undefined>;
}

/** A deck, its jobs and its open change sets, kept current by the server's events. */
export function useDeck(id: string): DeckHandle {
  const [state, setState] = useState<DeckState | null>(null);
  const [jobs, setJobs] = useState<Job[]>([]);
  const [changeSets, setChangeSets] = useState<ChangeSet[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    Promise.all([api.deck(id), api.jobs(id), api.changeSets(id)]).then(([s, j, c]) => {
      if (!live) return;
      setState(s);
      setJobs(j);
      setChangeSets(c);
    }, (e: Error) => live && setError(e.message));
    return () => {
      live = false;
    };
  }, [id]);

  useEvents((e) => {
    if (e.type === 'deck' && e.state.id === id) setState(e.state);
    if (e.type === 'job' && e.job.deckId === id) setJobs((js) => [e.job, ...js.filter((j) => j.id !== e.job.id)].sort((a, b) => b.createdAt - a.createdAt));
    if (e.type === 'changeset' && e.changeSet.deckId === id) {
      setChangeSets((cs) => (e.changeSet.status === 'open' ? [e.changeSet, ...cs.filter((c) => c.id !== e.changeSet.id)] : cs.filter((c) => c.id !== e.changeSet.id)));
    }
  });

  const act = useCallback(async <T,>(f: () => Promise<T>): Promise<T | undefined> => {
    try {
      setError(null);
      return await f();
    } catch (e) {
      setError((e as Error).message);
      return undefined;
    }
  }, []);

  const edit = useCallback(async (ops: Op[]) => {
    const s = await act(() => api.ops(id, ops));
    if (s) setState(s);
    return !!s;
  }, [act, id]);

  return { state, jobs, changeSets, error, setError, edit, act };
}
