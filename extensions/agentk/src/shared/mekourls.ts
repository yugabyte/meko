// Meko Cloud's addresses, and the Meko web app that goes with an MCP server URL. Shared by the
// server (which links datapacks and shares through the web app) and the web UI (the Meko tab's
// sign-up links follow what's typed, before it's saved), so both work it out the same way.

/** Meko Cloud's MCP server, used when neither config, the environment nor Claude Code's settings name one. */
export const DEFAULT_MEKO_URL = 'https://mcp.mekodata.ai/mcp';
/** Meko Cloud's web app, which goes with DEFAULT_MEKO_URL. */
export const DEFAULT_MEKO_WEB_URL = 'https://cloud.mekodata.ai';

/**
 * Installs Meko on this computer: the installer of the repo's latest release, which is Meko's
 * (the README's install line). The banner shown while there's no Meko here gives it.
 */
export const MEKO_INSTALL_COMMAND = 'gh release download --repo yugabyte/agent-k-local --pattern install.sh --output - | sh';

/**
 * The Meko web app that goes with an MCP URL: Meko Cloud's for Meko Cloud, else null (a local or
 * self-hosted server, whose web app can't be known). An explicit override always wins.
 */
export function mekoWebUrl(mcpUrl: string, override = ''): string | null {
  if (override.trim()) return override.trim().replace(/\/+$/, '');
  try {
    return new URL(mcpUrl).hostname === new URL(DEFAULT_MEKO_URL).hostname ? DEFAULT_MEKO_WEB_URL : null;
  } catch {
    return null;
  }
}

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
