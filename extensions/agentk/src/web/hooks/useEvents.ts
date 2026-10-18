import { useEffect, useRef, useState } from 'react';
import type { AppState } from '../../shared/types';
import { api, checkLogin, wsUrl, WS_UNAUTHORIZED } from '../api';
import { EventsConnection, type Conn } from '../events';
import { knowledgeNotice } from '../knowledge';
import { peekTerminal } from '../terminals';

export type { Conn };

export interface NotifyEvent {
  projectId: string;
  sessionId: string;
  title: string;
  body: string;
}

/** A project datapack was shared with us. */
export interface InviteEvent {
  datapackId: string;
  title: string;
  body: string;
}

/**
 * Live AppState from /ws/events (see EventsConnection: backoff, and checking a quiet socket or
 * one whose page was just shown again). Falls back to GET /api/state for the first paint. A
 * close for a lost login (4401) goes to the login screen instead of retrying.
 */
export function useEvents(
  onNotify: (n: NotifyEvent) => void,
  onInvite: (n: InviteEvent) => void,
): { state: AppState | null; conn: Conn } {
  const [state, setState] = useState<AppState | null>(null);
  const [conn, setConn] = useState<Conn>('connecting');
  const notifyRef = useRef(onNotify);
  notifyRef.current = onNotify;
  const inviteRef = useRef(onInvite);
  inviteRef.current = onInvite;

  useEffect(() => {
    let stopped = false;
    api
      .state()
      .then((s) => {
        if (!stopped) setState((cur) => cur ?? s);
      })
      .catch(() => {
        /* the socket will deliver state, or the 401 handler takes over */
      });

    const events = new EventsConnection({
      open: () => new WebSocket(wsUrl('/ws/events')),
      onState: setState,
      onConn: setConn,
      onEvent: (ev) => {
        if (ev.type === 'notify') notifyRef.current(ev);
        else if (ev.type === 'invite') inviteRef.current(ev);
        // How a file added to the knowledge base indexed: said in its session's terminal, if this page has it.
        else peekTerminal(ev.sessionId)?.showNotice(knowledgeNotice(ev), 8000);
      },
      checkLogin,
      unauthorized: WS_UNAUTHORIZED,
    });
    events.start();
    // A phone's page that was in the background may hold a socket the server has long dropped.
    const resume = () => {
      if (!document.hidden) events.resume();
    };
    document.addEventListener('visibilitychange', resume);
    window.addEventListener('pageshow', resume);
    window.addEventListener('online', resume);

    return () => {
      stopped = true;
      events.stop();
      document.removeEventListener('visibilitychange', resume);
      window.removeEventListener('pageshow', resume);
      window.removeEventListener('online', resume);
    };
  }, []);

  return { state, conn };
}
