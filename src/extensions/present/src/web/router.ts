import { useEffect, useState } from 'react';
import { BASE } from './base.js';

export type Route =
  | { view: 'decks' }
  | { view: 'new' }
  | { view: 'map'; deck: string }
  | { view: 'slides'; deck: string; slide: string | null }
  | { view: 'console'; run: string }
  | { view: 'audience'; run: string };

/** The route a path names, relative to where the pages are served. */
export function parseRoute(pathname: string, base: string = BASE): Route {
  const rest = pathname.startsWith(base) ? pathname.slice(base.length) : pathname.replace(/^\//, '');
  const p = rest.split('/').filter(Boolean).map(decodeURIComponent);
  if (p[0] === 'new') return { view: 'new' };
  if (p[0] === 'decks' && p[1] && p[2] === 'map') return { view: 'map', deck: p[1] };
  if (p[0] === 'decks' && p[1] && p[2] === 'slides') return { view: 'slides', deck: p[1], slide: p[3] ?? null };
  if (p[0] === 'runs' && p[1] && p[2] === 'console') return { view: 'console', run: p[1] };
  if (p[0] === 'runs' && p[1] && p[2] === 'audience') return { view: 'audience', run: p[1] };
  return { view: 'decks' };
}

export function routePath(r: Route, base: string = BASE): string {
  const e = encodeURIComponent;
  switch (r.view) {
    case 'decks': return base;
    case 'new': return `${base}new`;
    case 'map': return `${base}decks/${e(r.deck)}/map`;
    case 'slides': return `${base}decks/${e(r.deck)}/slides${r.slide ? `/${e(r.slide)}` : ''}`;
    case 'console': return `${base}runs/${e(r.run)}/console`;
    case 'audience': return `${base}runs/${e(r.run)}/audience`;
  }
}

export function navigate(r: Route, replace = false): void {
  const path = routePath(r);
  if (path === location.pathname) return;
  if (replace) history.replaceState(null, '', path);
  else history.pushState(null, '', path);
  dispatchEvent(new PopStateEvent('popstate'));
}

export function useRoute(): Route {
  const [route, setRoute] = useState(() => parseRoute(location.pathname));
  useEffect(() => {
    const on = () => setRoute(parseRoute(location.pathname));
    addEventListener('popstate', on);
    return () => removeEventListener('popstate', on);
  }, []);
  return route;
}
