// The Meko Cloud link's URLs as the page works them out while they're typed, before they're saved:
// the same way Meko's server does once they are (internal/cloudlink).

// The web app that goes with an MCP URL: Meko Cloud's for Meko Cloud, else null (another server's
// web app can't be known). An override always wins.
export function mekoWebUrl(mcpUrl: string, override: string, cloud: { default_url: string; default_web_url: string }): string | null {
  if (override.trim()) return override.trim().replace(/\/+$/, "");
  try {
    return new URL(mcpUrl).hostname === new URL(cloud.default_url).hostname ? cloud.default_web_url : null;
  } catch {
    return null;
  }
}

// A URL's host, or null when it isn't an http(s) URL.
export function httpHost(url: string | null | undefined): string | null {
  if (!url) return null;
  try {
    const u = new URL(url);
    return u.protocol === "https:" || u.protocol === "http:" ? u.host : null;
  } catch {
    return null;
  }
}

// Where to sign up, sign in and make a key on a Meko web app; null when it isn't an http(s) URL.
export function mekoKeyLinks(webUrl: string | null): { signup: string; signin: string; settings: string } | null {
  const base = webUrl?.trim().replace(/\/+$/, "") ?? "";
  if (!httpHost(base)) return null;
  const back = `return_to=${encodeURIComponent("/settings")}`;
  return { signup: `${base}/signup?${back}`, signin: `${base}/signin?${back}`, settings: `${base}/settings` };
}
