// Meko Cloud's addresses, and the Meko web app that goes with an MCP server URL. Shared by the
// server (which links datapacks and shares through the web app) and the web UI (the Meko tab's
// sign-up links follow what's typed, before it's saved), so both work it out the same way.

/** Meko Cloud's MCP server, used when neither config, the environment nor Claude Code's settings name one. */
export const DEFAULT_MEKO_URL = 'https://mcp.mekodata.ai/mcp';
/** Meko Cloud's web app, which goes with DEFAULT_MEKO_URL. */
export const DEFAULT_MEKO_WEB_URL = 'https://cloud.mekodata.ai';

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
