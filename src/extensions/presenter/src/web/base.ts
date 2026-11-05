/**
 * The path Presenter's pages are served under, always ending in '/': `/ext/presenter/` behind Meko,
 * whose `<base href>` Presenter's server stamps into index.html, and `/` under `npm run dev`, where
 * Vite serves the page with no `<base>`.
 */
export function basePath(baseURI: string | null): string {
  if (!baseURI) return '/';
  const path = new URL(baseURI).pathname;
  return path.slice(0, path.lastIndexOf('/') + 1) || '/';
}

export const BASE = basePath(
  typeof document !== 'undefined' && document.querySelector('base[href]') ? document.baseURI : null,
);

/** A URL on Presenter's own server (`/api/decks`, `logo.png`): under BASE, wherever the page is served. */
export function appUrl(path: string): string {
  return BASE + path.replace(/^\//, '');
}

/** A WebSocket URL on Presenter's own server. */
export function wsUrl(path: string): string {
  const u = new URL(appUrl(path), location.href);
  u.protocol = u.protocol === 'https:' ? 'wss:' : 'ws:';
  return u.toString();
}
