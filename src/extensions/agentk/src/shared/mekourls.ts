// Meko Cloud's addresses, the Meko web app that goes with an MCP server URL, and Meko's own pages.
// Shared by the server (which links datapacks and shares through the web app) and the web UI.

/**
 * Meko Cloud's MCP server: the one Meko's plugin for Claude Code uses when Claude Code's settings
 * name none (`mekosetup.ts`).
 */
export const DEFAULT_MEKO_URL = 'https://mcp.mekodata.ai/mcp';
/** Meko Cloud's web app: the host PR descriptions name when Meko names no web app and none runs here (`mekoHost`). */
export const DEFAULT_MEKO_WEB_URL = 'https://cloud.mekodata.ai';

/**
 * Where browsers find Kode: the Meko on this computer serves it at this path on its web listener,
 * with the path stripped (Meko's internal/extproxy), so Kode's own routes stay at `/`.
 */
export const KODE_PATH = '/ext/kode/';

/** Meko's main page, on the same web listener as Kode: the projects pane's Meko "m" links to it. */
export const MEKO_HOME_PATH = '/';

/**
 * A datapack's page in the web UI of the Meko on this computer (its overview tab), on the same
 * origin as Kode. Datapack names link to it: that web UI opens a Meko Cloud datapack in Meko
 * Cloud's web app.
 */
export function mekoDatapackPath(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}`;
}

/**
 * A datapack's Context Gaps in that web UI, the questions Meko couldn't answer: its tab, which that
 * web UI sends on to its page of their own for a Meko Cloud datapack.
 */
export function mekoGapsPath(datapackId: string): string {
  return `${mekoDatapackPath(datapackId)}/gaps`;
}

/**
 * Installs Meko on this computer: the installer of the repo's latest release, which is Meko's
 * (the README's install line). The banner shown while there's no Meko here gives it.
 */
export const MEKO_INSTALL_COMMAND = 'curl -fsSL https://mekodata.ai/install.sh | sh';

/** A datapack's share link: the web app's own `/share/{token}` page, where Meko signs the person in and joins them. */
export function shareLinkUrl(webUrl: string, token: string): string {
  return `${webUrl.replace(/\/+$/, '')}/share/${encodeURIComponent(token)}`;
}

/**
 * The token of a share link pasted as `text`: only a link on this Meko's web app (same origin and
 * path), so a look-alike link is never sent to Meko. Null for anything else.
 */
export function shareLinkToken(text: string, webUrl: string | null): string | null {
  if (!webUrl) return null;
  let link: URL;
  let base: URL;
  try {
    link = new URL(text.trim());
    base = new URL(webUrl);
  } catch {
    return null;
  }
  if (!/^https?:$/.test(link.protocol) || link.origin !== base.origin) return null;
  const prefix = `${base.pathname.replace(/\/+$/, '')}/share/`;
  if (!link.pathname.startsWith(prefix)) return null;
  const rest = link.pathname.slice(prefix.length).replace(/\/$/, '');
  if (!rest || rest.includes('/')) return null;
  try {
    return decodeURIComponent(rest) || null;
  } catch {
    return null;
  }
}
