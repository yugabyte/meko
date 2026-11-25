import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { Deck } from '../shared/deck.js';
import { fitRun, type NavAction, type RunState } from '../shared/run.js';
import type { RunServerMessage } from '../shared/types.js';
import { api } from './api.js';
import { wsUrl } from './base.js';

/** A live run's room: the deck and the position, pushed by the server, and the way to move it. */
export function useRun(runId: string) {
  const [deck, setDeck] = useState<Deck | null>(null);
  const [raw, setRun] = useState<RunState | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [connected, setConnected] = useState(false);
  const ws = useRef<WebSocket | null>(null);

  // The run over HTTP first, so the slide shows while the socket connects (or can't, behind a proxy).
  useEffect(() => {
    let live = true;
    api.run(runId).then((r) => {
      if (!live) return;
      setDeck((d) => d ?? r.deck);
      setRun((x) => x ?? r.run);
    }, (e: Error) => live && setError(e.message));
    return () => {
      live = false;
    };
  }, [runId]);

  useEffect(() => {
    let closed = false;
    let retry: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    const connect = () => {
      if (closed) return;
      const s = new WebSocket(wsUrl(`/ws/runs/${encodeURIComponent(runId)}`));
      ws.current = s;
      s.onopen = () => ((delay = 500), setConnected(true), setError(null));
      s.onmessage = (m) => {
        let msg: RunServerMessage;
        try {
          msg = JSON.parse(String(m.data)) as RunServerMessage;
        } catch {
          return;
        }
        if (msg.type === 'deck') setDeck(msg.deck);
        else if (msg.type === 'run') setRun(msg.run);
        else setError(msg.error);
      };
      // A restart or a sign-out drops the socket: the last slide stays up while it comes back.
      s.onclose = () => {
        setConnected(false);
        if (closed) return;
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 10000);
      };
    };
    connect();
    return () => {
      closed = true;
      if (retry) clearTimeout(retry);
      ws.current?.close();
    };
  }, [runId]);

  const nav = useCallback((n: NavAction) => {
    if (ws.current?.readyState === WebSocket.OPEN) ws.current.send(JSON.stringify({ type: 'nav', nav: n }));
  }, []);

  // The deck and the run arrive as separate messages: the run is shown fitted to the deck this page
  // has, so a position in a scene it doesn't have yet (or any more) shows the nearest one meanwhile.
  const run = useMemo(() => (deck && raw ? fitRun(deck, raw) : raw), [deck, raw]);

  return { deck, run, error, connected, nav };
}

/** The keys both windows take: arrows and space move, 1–9 choose, B blanks. */
export function navForKey(e: KeyboardEvent, blank: boolean): NavAction | null {
  if (e.metaKey || e.ctrlKey || e.altKey) return null;
  if (e.key === 'ArrowRight' || e.key === ' ' || e.key === 'PageDown') return { action: 'next' };
  if (e.key === 'ArrowLeft' || e.key === 'PageUp') return { action: 'back' };
  if (/^[1-9]$/.test(e.key)) return { action: 'choose', key: e.key };
  if (e.key === 'b' || e.key === 'B' || e.key === '.') return { action: 'blank', on: !blank };
  return null;
}
