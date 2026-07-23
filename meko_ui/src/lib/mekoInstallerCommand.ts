import { MEKO_TOKEN_INSTALLER_PLACEHOLDER } from "@/lib/mekoTokens";

/** `create-meko-setup.mjs --client` values from install docs. */
export type MekoInstallerClient = "claude-desktop" | "claude-code" | "cursor" | "codex";

/**
 * One-line installer command for MCP setup (matches McpInstallGuide examples).
 *
 * The `--api-key` value is the {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER}
 * literal — never a real plaintext token. The Connect dialog instructs the
 * user to substitute their own saved token (we never recover plaintexts
 * after creation, so the UI can't fill it in).
 */
export function buildMekoInstallerCommand(
  client: MekoInstallerClient,
  mcpServerUrl: string,
): string {
  return `npx @yugabytedb/meko-mcp --client ${client} --url ${JSON.stringify(mcpServerUrl)} --api-key ${MEKO_TOKEN_INSTALLER_PLACEHOLDER}`;
}
