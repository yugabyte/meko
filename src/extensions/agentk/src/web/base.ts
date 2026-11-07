/**
 * The path Kode's pages are served under, always ending in '/': `/ext/kode/` behind Meko,
 * which serves Kode there and whose `<base href>` Kode's server stamps into index.html, and
 * `/` under `npm run dev`, where Vite serves the page with no `<base>` (then `document.baseURI` is
 * the page's own URL, whose path is a route, not the base).
 */
export function basePath(baseURI: string | null): string {
  if (!baseURI) return '/';
  const path = new URL(baseURI).pathname;
  return path.slice(0, path.lastIndexOf('/') + 1) || '/';
}

export const BASE = basePath(
  typeof document !== 'undefined' && document.querySelector('base[href]') ? document.baseURI : null,
);

/** Where Meko served Kode before it called it Kode, which now sends browsers to BASE. */
export const OLD_BASE = '/ext/agentk/';

/** A URL on Kode's own server (`/api/state`, `/logo.png`): under BASE, wherever the page is served. */
export function appUrl(path: string): string {
  return BASE + path.replace(/^\//, '');
}
