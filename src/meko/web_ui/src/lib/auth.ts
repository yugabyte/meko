// The addresses of pages Meko had before: the sign-in screens and account page, from before it
// dropped its login, and a datapack's Connect tab, from before the Agents page. Bookmarks and older
// extensions' pages still link to them, so each leads into the app.

export const SIGNIN_PATH = "/signin";
export const SETUP_PATH = "/setup";
export const ACCOUNT_PATH = "/account";

// Where next leads: a page on this site, else the app's start. It's read the way the browser will
// read it, so "//host", "/\host" or a path with a tab or newline in it, which browsers take as
// another site's address, can't send anyone elsewhere.
export function safeNext(next: string | null): string {
  if (!next || !next.startsWith("/")) return "/";
  let u: URL;
  try {
    u = new URL(next, window.location.origin);
  } catch {
    return "/";
  }
  return u.origin === window.location.origin ? u.pathname + u.search + u.hash : "/";
}

// A datapack's old Connect tab: /datapacks/<id>/connect.
const CONNECT_TAB = /^\/datapacks\/[^/]+\/connect\/?$/;

// The address an old one goes to (/signin?next=<path> to next, /setup to the start, /account to
// Settings, a datapack's Connect tab to Agents), or null for any other.
export function oldAddressTarget(path: string, search: string): string | null {
  if (path === SIGNIN_PATH) return safeNext(new URLSearchParams(search).get("next"));
  if (path === SETUP_PATH) return "/";
  if (path === ACCOUNT_PATH) return "/settings";
  if (CONNECT_TAB.test(path)) return "/agents";
  return null;
}
