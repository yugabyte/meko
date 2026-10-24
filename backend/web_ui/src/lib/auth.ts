// The sign-in screens' paths: /signin?next=<path> and /setup?token=<token>, outside the app's own
// routes (lib/route).

export const SIGNIN_PATH = "/signin";
export const SETUP_PATH = "/setup";

// Where a sign-in goes on to: next when it's a page on this site, else the app's start. It's read
// the way the browser will read it, so "//host", "/\host" or a path with a tab or newline in it,
// which browsers take as another site's address, can't send a signed-in user elsewhere.
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

// The sign-in screen's address, coming back to the page shown now.
export function signinHref(): string {
  const here = window.location.pathname + window.location.search;
  const next = here.startsWith(SIGNIN_PATH) || here.startsWith(SETUP_PATH) ? "/" : here;
  return `${SIGNIN_PATH}?next=${encodeURIComponent(next)}`;
}
