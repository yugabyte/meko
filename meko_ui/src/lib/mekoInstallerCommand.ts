/** `create-meko-setup.mjs --client` values from install docs. */
export type MekoInstallerClient = "claude-desktop" | "claude-code" | "cursor";

/**
 * One-line installer command for MCP setup (matches McpInstallGuide examples; JSON-quoted URL/key for safe copy).
 */
export function buildMekoInstallerCommand(
  client: MekoInstallerClient,
  mcpServerUrl: string,
  mcpApiKey: string,
): string {
  return `npx @yugabytedb/meko-mcp --client ${client} --url ${JSON.stringify(mcpServerUrl)} --api-key ${JSON.stringify(mcpApiKey)}`;
}
