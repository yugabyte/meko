import { useEffect, useRef } from 'react';
import type { ServerEvent } from '../shared/types.js';
import { wsUrl } from './base.js';

/** Every server event while the page is open, reconnecting after a drop (a restart, a sign-out). */
export function useEvents(on: (e: ServerEvent) => void): void {
  const ref = useRef(on);
  useEffect(() => {
    ref.current = on;
  });
  useEffect(() => {
    let ws: WebSocket | null = null;
    let closed = false;
    let retry: ReturnType<typeof setTimeout> | null = null;
    let delay = 500;
    const connect = () => {
      if (closed) return;
      ws = new WebSocket(wsUrl('/ws/events'));
      ws.onopen = () => (delay = 500);
      ws.onmessage = (m) => {
        try {
          ref.current(JSON.parse(String(m.data)) as ServerEvent);
        } catch {
          /* not an event */
        }
      };
      ws.onclose = () => {
        if (closed) return;
        retry = setTimeout(connect, delay);
        delay = Math.min(delay * 2, 10000);
      };
    };
    connect();
    return () => {
      closed = true;
      if (retry) clearTimeout(retry);
      ws?.close();
    };
  }, []);
}
