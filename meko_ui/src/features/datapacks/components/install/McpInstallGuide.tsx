/**
 * MCP client install guide: Claude Code, Claude Desktop, Cursor, and manual MCP install with copyable shell steps.
 * Shared by the Home “Install” page and the datapack configure wizard’s Install step.
 *
 * The API key is NEVER injected from provision-status anymore (the legacy
 * datapack-scoped `mcp_api_key` flow). Templates render with the
 * {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER} literal so users substitute a
 * Meko personal access token they minted in Settings → Security.
 */
import { useMemo, useState } from "react";
import { Check, Copy, Code2, Monitor, MousePointer2, Terminal } from "lucide-react";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";
import { useTruncatableCodeDisplay } from "@/hooks/useTruncatableCodeDisplay";
import { MEKO_TOKEN_INSTALLER_PLACEHOLDER } from "@/lib/mekoTokens";

/** Bash-safe single-quoted literal for use in generated install commands. */
function shellSingleQuoted(value: string): string {
  return `'${value.replace(/'/g, `'\\''`)}'`;
}

/**
 * When a URL is provided, swap URL placeholders for the quoted value. API
 * key placeholders are normalized to {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER}
 * (and never substituted with a real key — see file header).
 */
function injectMcpProvisionIntoCode(code: string, mcpServerUrl?: string | null): string {
  const url = mcpServerUrl?.trim();
  const withKey = code
    .replaceAll("<mcp-server-api-key>", MEKO_TOKEN_INSTALLER_PLACEHOLDER)
    .replaceAll("<meko-server-api-key>", MEKO_TOKEN_INSTALLER_PLACEHOLDER);
  if (!url) return withKey;
  const qUrl = shellSingleQuoted(url);
  return withKey
    .replaceAll("<mcp-server-url>", qUrl)
    .replaceAll("<meko-mcp-url>", qUrl);
}

/** Collapsed install-step code preview; copy always uses the full injected command. */
const INSTALL_GUIDE_CODE_BLOCK_PREVIEW_MAX_CHARS = 320;

type IdeTab = "claude_code" | "claude_desktop" | "cursor" | "manual_mcp";

type InstallStep = {
  title: string;
  description: string;
  code?: string;
  langLabel?: string;
  /** NVM-style one-liner: single row + horizontal scroll (default wraps long lines). */
  codeSingleRowScroll?: boolean;
};

/** Shared first step for Claude Code, Claude Desktop, and Cursor tabs (npx requires Node 18+). */
const SHARED_NVM_NODE_PREREQ_STEP: InstallStep = {
  title: "Install NVM and Node.js",
  description:
    "Install Node Version Manager (nvm) so you can run npx in the next step. Use **Node.js 18.0 or newer** (the current LTS is recommended).",
  code: [
    "curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash",
    "source ~/.$(basename $SHELL)rc",
    "nvm install --lts",
    "nvm use --lts",
    "node -v  # should print v18.x or newer",
  ].join(" && "),
  langLabel: "bash",
  codeSingleRowScroll: true,
};

const CLAUDE_CODE_INSTALL_STEPS: InstallStep[] = [
  SHARED_NVM_NODE_PREREQ_STEP,
  {
    title: "Install Meko MCP for Claude Code",
    description:
      "Run once with your MCP URL and API key from the Datapacks page (replace the placeholders before running).",
    code: "npx @yugabytedb/meko-mcp --client claude-code --url <mcp-server-url> --api-key <mcp-server-api-key>",
    langLabel: "bash",
  },
];

const CLAUDE_DESKTOP_INSTALL_STEPS: InstallStep[] = [
  SHARED_NVM_NODE_PREREQ_STEP,
  {
    title: "Install Meko MCP for Claude Desktop",
    description:
      "Run once with your MCP URL and API key from the Datapacks page (replace the placeholders before running).",
    code: "npx @yugabytedb/meko-mcp --client claude-desktop --url <mcp-server-url> --api-key <mcp-server-api-key>",
    langLabel: "bash",
  },
];

const CURSOR_INSTALL_STEPS: InstallStep[] = [
  SHARED_NVM_NODE_PREREQ_STEP,
  {
    title: "Install Meko MCP for Cursor",
    description:
      "Run once with your MCP URL and API key from the Datapacks page (replace the placeholders before running).",
    code: "npx @yugabytedb/meko-mcp --client cursor --url <mcp-server-url> --api-key <mcp-server-api-key>",
    langLabel: "bash",
  },
];

const MANUAL_MCP_INSTALL_STEPS: InstallStep[] = [
  {
    title: "Install NVM",
    description:
      "Install Node Version Manager so you can run the tooling used to register the MCP server with your assistant.",
    code: "curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash",
    langLabel: "bash",
  },
  {
    title: "Load NVM in your shell",
    description:
      "Source your shell configuration so nvm is available in the current session (adjust the file if you use a custom profile).",
    code: "source ~/.$(basename $SHELL)rc",
    langLabel: "bash",
  },
  {
    title: "Integrate MCP server",
    description:
      "Run mcp-add with your Meko MCP URL and API keys from the Datapacks page. Replace placeholders before executing.",
    code: [
      'echo "### Installing the latest Node.js ###"',
      "curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash",
      "source ~/.$(basename $SHELL)rc",
      "nvm install --lts",
      'echo "### Registering the Meko MCP server ###"',
      "npx mcp-add@latest \\",
      "  --name meko \\",
      "  --type stdio \\",
      "  --command 'npx -y mcp-remote <mcp-server-url> --allow-http' \\",
      "  --env 'MEKO_API_KEY=<mcp-server-api-key>' \\",
      "  --headers 'MEKO_API_KEY=<mcp-server-api-key>' \\",
      "  --scope global \\",
      "  --clients 'claude desktop,claude code,cursor,codex,vscode'",
    ].join("\n"),
    langLabel: "bash",
  },
  {
    title: "Integrate skills package with the AI agent",
    description:
      "Integrate the skills package with your AI agent to learn how to use Meko's MCP tools for memory, knowledge, and more.",
    code: [
      "git clone https://github.com/yugabyte/meko-mcp-server.git",
      "cd meko-mcp-server",
      "git checkout add-skills",
      "npx skills add ./skills -g",
    ].join("\n"),
    langLabel: "bash",
  },
  {
    title: "Select skills at the prompt",
    description:
      "When prompted to select the skills to install, select both meko-mcp-tools and ysql.",
  },
];

function buildInstallGuide(
  mcpServerUrl?: string | null,
): Record<IdeTab, InstallStep[]> {
  const inject = (steps: InstallStep[]) =>
    steps.map((step) => ({
      ...step,
      code: step.code != null ? injectMcpProvisionIntoCode(step.code, mcpServerUrl) : undefined,
    }));

  return {
    claude_code: inject(CLAUDE_CODE_INSTALL_STEPS),
    claude_desktop: inject(CLAUDE_DESKTOP_INSTALL_STEPS),
    cursor: inject(CURSOR_INSTALL_STEPS),
    manual_mcp: inject(MANUAL_MCP_INSTALL_STEPS),
  };
}

const TAB_META: { id: IdeTab; label: string; icon: typeof Code2 }[] = [
  { id: "claude_code", label: "Claude Code", icon: Code2 },
  { id: "claude_desktop", label: "Claude Desktop", icon: Monitor },
  { id: "cursor", label: "Cursor", icon: MousePointer2 },
  { id: "manual_mcp", label: "Manual MCP Install", icon: Terminal },
];

function CodeBlock({
  code,
  langLabel,
  copyId,
  copiedId,
  onCopy,
  codeSingleRowScroll = false,
}: {
  code: string;
  langLabel: string;
  copyId: string;
  copiedId: string | null;
  onCopy: (text: string, id: string) => void;
  codeSingleRowScroll?: boolean;
}) {
  // Long-API-key middle-ellipsis is no longer needed — the API key in
  // every template is the short {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER}
  // literal. Char-count truncation still kicks in for the multi-line
  // manual-MCP block.
  const { displayText, needsToggle, expanded, toggleExpanded } = useTruncatableCodeDisplay(
    code,
    INSTALL_GUIDE_CODE_BLOCK_PREVIEW_MAX_CHARS,
    code,
  );

  return (
    <div className="max-w-full overflow-hidden rounded-lg border border-border bg-input/80 shadow-sm">
      <div className="flex items-center justify-between gap-2 border-b border-border bg-muted/40 px-3 py-2">
        <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          {langLabel}
        </span>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="h-8 w-8 shrink-0 text-muted-foreground hover:text-foreground"
          onClick={() => onCopy(code, copyId)}
          aria-label="Copy code"
        >
          {copiedId === copyId ? (
            <Check className="h-4 w-4 text-green-500" />
          ) : (
            <Copy className="h-4 w-4" />
          )}
        </Button>
      </div>
      <div className="p-4 pt-3">
        <pre className="m-0 max-w-full overflow-x-auto text-sm leading-relaxed text-foreground/95">
          <code
            className={cn(
              "block font-mono",
              codeSingleRowScroll
                ? "whitespace-pre w-max min-w-full"
                : "whitespace-pre-wrap break-words [overflow-wrap:anywhere]",
            )}
          >
            {displayText}
          </code>
        </pre>
        {needsToggle ? (
          <button
            type="button"
            className="mt-2 text-sm font-medium text-primary hover:underline"
            onClick={toggleExpanded}
          >
            {expanded ? "Show less" : "Show all"}
          </button>
        ) : null}
      </div>
    </div>
  );
}

function InstallSteps({ tab, steps }: { tab: IdeTab; steps: InstallStep[] }) {
  const [copiedId, setCopiedId] = useState<string | null>(null);

  const handleCopy = async (text: string, id: string) => {
    const ok = await copyToClipboard(text);
    if (!ok) return;
    setCopiedId(id);
    window.setTimeout(() => setCopiedId(null), 2000);
  };

  return (
    <div className="relative space-y-10">
      <div
        className="pointer-events-none absolute bottom-5 left-[17px] top-5 hidden w-px bg-border lg:block"
        aria-hidden
      />
      {steps.map((step, index) => {
        const n = index + 1;
        const copyId = `${tab}-${n}`;
        const code = step.code?.trim() ?? "";
        const showCode = code.length > 0;
        return (
          <div
            key={`${tab}-${n}-${step.title}`}
            className={cn(
              "grid gap-6 lg:items-start",
              showCode ? "lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)] lg:gap-10" : "",
            )}
          >
            <div className="relative flex gap-4 pl-1">
              <div className="relative z-[1] flex flex-col items-center">
                <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full border-2 border-border bg-background text-sm font-semibold text-foreground shadow-sm">
                  {n}
                </div>
              </div>
              <div className="min-w-0 space-y-2 pt-0.5">
                <h3 className="text-base font-semibold tracking-tight text-foreground">{step.title}</h3>
                <p className="text-sm leading-relaxed text-muted-foreground">{step.description}</p>
              </div>
            </div>
            {showCode ? (
              <div className="min-w-0">
                <CodeBlock
                  code={code}
                  langLabel={step.langLabel ?? "bash"}
                  copyId={copyId}
                  copiedId={copiedId}
                  onCopy={handleCopy}
                  codeSingleRowScroll={step.codeSingleRowScroll === true}
                />
              </div>
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

type McpInstallGuideProps = {
  className?: string;
  /**
   * When provided (e.g. from `getDatapackProvisionStatus`), `<mcp-server-url>`
   * placeholders are replaced with the bash-quoted value. Omit on pages
   * like Home where the user has no datapack context yet.
   *
   * The API key is NEVER injected — it's always rendered as
   * {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER} for the user to paste in.
   */
  mcpServerUrl?: string | null;
};

/**
 * MCP install steps (Claude Code / Claude Desktop / Cursor / Manual MCP Install).
 */
export function McpInstallGuide({ className, mcpServerUrl = null }: McpInstallGuideProps) {
  const installGuide = useMemo(
    () => buildInstallGuide(mcpServerUrl),
    [mcpServerUrl],
  );

  return (
    <Tabs defaultValue="claude_code" className={cn("w-full", className)}>
      <TabsList className="flex h-auto w-full flex-wrap justify-start gap-1 rounded-xl bg-muted/60 p-1.5 sm:inline-flex sm:w-auto">
        {TAB_META.map(({ id, label, icon: Icon }) => (
          <TabsTrigger
            key={id}
            value={id}
            className={cn(
              "gap-2 rounded-lg px-4 py-2.5 text-sm data-[state=active]:bg-background data-[state=active]:text-foreground data-[state=active]:shadow-md data-[state=inactive]:text-muted-foreground",
            )}
          >
            <Icon className="h-4 w-4 shrink-0 opacity-80" />
            {label}
          </TabsTrigger>
        ))}
      </TabsList>

      {TAB_META.map(({ id }) => (
        <TabsContent key={id} value={id} className="mt-6 rounded-xl border border-border bg-card/30 p-6 sm:p-8">
          <InstallSteps tab={id} steps={installGuide[id]} />
        </TabsContent>
      ))}
    </Tabs>
  );
}
