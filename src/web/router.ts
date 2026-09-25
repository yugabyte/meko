import { useEffect, useState } from 'react';

export type Route =
  | { view: 'root' }
  | { view: 'project'; projectId: string; sessionId: string | null }
  | { view: 'home'; projectId: string };

export function parseRoute(pathname: string): Route {
  const parts = pathname.split('/').filter(Boolean).map(decodeURIComponent);
  if (parts[0] === 'p' && parts[1]) {
    if (parts[2] === 's' && parts[3]) return { view: 'project', projectId: parts[1], sessionId: parts[3] };
    if (parts[2] === 'home') return { view: 'home', projectId: parts[1] };
    return { view: 'project', projectId: parts[1], sessionId: null };
  }
  return { view: 'root' };
}

export function routePath(r: Route): string {
  const e = encodeURIComponent;
  switch (r.view) {
    case 'root':
      return '/';
    case 'home':
      return `/p/${e(r.projectId)}/home`;
    case 'project':
      return r.sessionId ? `/p/${e(r.projectId)}/s/${e(r.sessionId)}` : `/p/${e(r.projectId)}`;
  }
}

const listeners = new Set<() => void>();

export function navigate(r: Route, opts: { replace?: boolean } = {}) {
  const path = routePath(r);
  if (path === location.pathname) return;
  if (opts.replace) history.replaceState(null, '', path);
  else history.pushState(null, '', path);
  listeners.forEach((l) => l());
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseRoute(location.pathname));
  useEffect(() => {
    const update = () => setRoute(parseRoute(location.pathname));
    listeners.add(update);
    window.addEventListener('popstate', update);
    return () => {
      listeners.delete(update);
      window.removeEventListener('popstate', update);
    };
  }, []);
  return route;
}
