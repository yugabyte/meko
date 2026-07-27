import { describe, it, expect } from "vitest";
import { buildMekoInstallerCommand } from "@/lib/mekoInstallerCommand";
import { MEKO_TOKEN_INSTALLER_PLACEHOLDER } from "@/lib/mekoTokens";

describe("buildMekoInstallerCommand", () => {
  it("embeds the client, the JSON-quoted URL, and the token placeholder", () => {
    const cmd = buildMekoInstallerCommand("claude-code", "https://mcp.example.com/x");
    expect(cmd).toBe(
      `npx @yugabytedb/meko-mcp --client claude-code --url "https://mcp.example.com/x" --api-key ${MEKO_TOKEN_INSTALLER_PLACEHOLDER}`,
    );
  });

  it("never emits a real token — always the placeholder", () => {
    const cmd = buildMekoInstallerCommand("cursor", "https://x");
    expect(cmd).toContain(MEKO_TOKEN_INSTALLER_PLACEHOLDER);
  });

  it("JSON-quotes the URL so odd characters can't break the command", () => {
    const cmd = buildMekoInstallerCommand("codex", 'https://x/"y"');
    expect(cmd).toContain('--url "https://x/\\"y\\""');
  });

  it("passes each supported client through verbatim", () => {
    for (const client of ["claude-desktop", "claude-code", "cursor", "codex"] as const) {
      expect(buildMekoInstallerCommand(client, "https://x")).toContain(`--client ${client}`);
    }
  });
});
