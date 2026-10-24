// The sign-in screens' paths: /signin?next=<path> and /setup?token=<token>, outside the app's own
// routes (lib/route).

export const SIGNIN_PATH = "/signin";
export const SETUP_PATH = "/setup";

// Where a sign-in goes on to: next when it's a path on this site, else the app's start. "//host"
// is another site's address, so it's refused too.
export function safeNext(next: string | null): string {
  return next && next.startsWith("/") && !next.startsWith("//") ? next : "/";
}

// The sign-in screen's address, coming back to the page shown now.
export function signinHref(): string {
  const here = window.location.pathname + window.location.search;
  const next = here.startsWith(SIGNIN_PATH) || here.startsWith(SETUP_PATH) ? "/" : here;
  return `${SIGNIN_PATH}?next=${encodeURIComponent(next)}`;
}
