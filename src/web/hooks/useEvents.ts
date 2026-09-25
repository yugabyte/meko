import { useEffect, useRef, useState } from 'react';
import type { AppState, ServerEvent } from '../../shared/types';
import { api, wsUrl } from '../api';

export type Conn = 'connecting' | 'open' | 'closed';

export interface NotifyEvent {
  projectId: string;
  sessionId: string;
  title: string;
  body: string;
}

/**
 * Live AppState from /ws/events. Reconnects with exponential backoff and
 * falls back to GET /api/state for the first paint.
 */
export function useEvents(onNotify: (n: NotifyEvent) => void): { state: AppState | null; conn: Conn } {
  const [state, setState] = useState<AppState | null>(null);
  const [conn, setConn] = useState<Conn>('connecting');
  const notifyRef = useRef(onNotify);
  notifyRef.current = onNotify;

  useEffect(() => {
    let ws: WebSocket | null = null;
    let timer: ReturnType<typeof setTimeout> | null = null;
    let attempt = 0;
    let stopped = false;

    api
      .state()
      .then((s) => {
        if (!stopped) setState((cur) => cur ?? s);
      })
      .catch(() => {
        /* the socket will deliver state, or the 401 handler takes over */
      });

    const connect = () => {
      if (stopped) return;
      setConn('connecting');
      ws = new WebSocket(wsUrl('/ws/events'));
      ws.onopen = () => {
        attempt = 0;
        setConn('open');
      };
      ws.onmessage = (ev) => {
        let msg: ServerEvent;
        try {
          msg = JSON.parse(ev.data as string);
        } catch {
          return;
        }
        if (msg.type === 'state') setState(msg.state);
        else if (msg.type === 'notify') notifyRef.current(msg);
      };
      ws.onclose = () => {
        ws = null;
        if (stopped) return;
        setConn('closed');
        const delay = Math.min(15000, 500 * 2 ** attempt++) + Math.random() * 300;
        timer = setTimeout(connect, delay);
      };
      ws.onerror = () => ws?.close();
    };
    connect();

    return () => {
      stopped = true;
      if (timer) clearTimeout(timer);
      ws?.close();
    };
  }, []);

  return { state, conn };
}
