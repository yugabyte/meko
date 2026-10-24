/**
 * The path AgentK's pages are served under, always ending in '/': `/ext/agentk/` behind Meko,
 * which serves AgentK there and whose `<base href>` AgentK's server stamps into index.html, and
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

/** A URL on AgentK's own server (`/api/state`, `/logo.png`): under BASE, wherever the page is served. */
export function appUrl(path: string): string {
  return BASE + path.replace(/^\//, '');
}
