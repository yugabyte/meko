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
import { Bot, Check, Code2, Copy, Globe, Monitor, MousePointer2, Terminal } from "lucide-react";
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
  return withKey.replaceAll("<mcp-server-url>", qUrl).replaceAll("<meko-mcp-url>", qUrl);
}

/** Collapsed install-step code preview; copy always uses the full injected command. */
const INSTALL_GUIDE_CODE_BLOCK_PREVIEW_MAX_CHARS = 320;

type IdeTab = "client_installer" | "manual_mcp" | "connector_oauth";

/**
 * Per-AI-tool metadata for the tool-picker that lives at the top of the
 * Client Installer + Connector (OAuth) tabs.
 *
 * `slug` is the literal value passed as `--client` to the
 * `@yugabytedb/meko-mcp` installer in the Client Installer flow, AND
 * the key used to look up paste instructions in the Connector (OAuth)
 * flow. The two flows share one selector state so the same chip choice
 * carries across both tabs.
 *
 * Icons today are lucide stand-ins (Monitor / Code2 / MousePointer2 /
 * Bot). If/when actual brand SVGs land in `meko_ui/public/` (mirroring
 * the existing `discord-icon.svg` convention — white-on-transparent,
 * tone via Tailwind `invert dark:invert-0`), swap the `Icon` field for
 * an `iconSrc: string` and render via `<img>`; the rest of the
 * structure stays identical.
 */
type ClientToolSlug = "claude-desktop" | "claude-code" | "cursor" | "codex";

type ClientTool = {
  slug: ClientToolSlug;
  label: string;
  Icon: typeof Code2;
  /**
   * Single-line "how to add Meko to this tool" hint shown as step 2 of
   * the Connector (OAuth) flow when this tool is selected. Wording
   * mirrors what the tool's actual UI calls things — "Settings →
   * Connectors → Add custom connector" for Claude Desktop, "claude mcp
   * add ..." for the CLI, etc.
   */
  connectorInstructions: string;
};

const CLIENT_TOOLS: ClientTool[] = [
  {
    slug: "claude-desktop",
    label: "Claude Desktop",
    Icon: Monitor,
    connectorInstructions: "Settings → Connectors → Add custom connector → paste URL",
  },
  {
    slug: "claude-code",
    label: "Claude Code",
    Icon: Code2,
    // Claude Code has no GUI Settings — adding an MCP server is a
    // single CLI invocation, mirrored here.
    connectorInstructions: "Run: claude mcp add meko <mcp-server-url> --transport http",
  },
  {
    slug: "cursor",
    label: "Cursor",
    Icon: MousePointer2,
    connectorInstructions: "Settings → MCP → Add new MCP server → Type: HTTP → paste URL",
  },
  {
    slug: "codex",
    label: "Codex",
    Icon: Bot,
    // Codex's MCP-server UI is still evolving — when the exact path
    // settles, update this string. The chip is wired up either way so
    // the layout is final.
    connectorInstructions: "Settings → MCP servers → Add → paste URL",
  },
];

const DEFAULT_CLIENT_TOOL: ClientTool = CLIENT_TOOLS[0];

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

// Single "Client Installer" tab covering all AI tools — the flow is
// identical for each, only the `--client` slug differs. Instead of one
// tab per tool (or one tab with a placeholder the user has to fill in),
// we render an icon-picker at the top of the tab and inject the
// selected tool's slug into the rendered `npx` command. Pick once,
// copy, paste.
function buildClientInstallerSteps(tool: ClientTool): InstallStep[] {
  return [
    SHARED_NVM_NODE_PREREQ_STEP,
    {
      title: `Install Meko MCP for ${tool.label}`,
      description:
        "Run once. The MCP URL is filled in below; substitute the API key placeholder with one from Settings → Security.",
      code: `npx @yugabytedb/meko-mcp --client ${tool.slug} --url <mcp-server-url> --api-key <mcp-server-api-key>`,
      langLabel: "bash",
    },
  ];
}

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
    title: "Integrate skills package with the AI tool",
    description:
      "Integrate the skills package with your AI tool to learn how to use Meko's MCP tools for memory, knowledge, and more.",
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

/**
 * "Remote (OAuth)" install path — no API key, no `npx` package.
 *
 * The user pastes the MCP server URL into their AI tool's "Add MCP
 * server / custom connector" UI. The tool detects the OAuth metadata
 * Meko publishes at `/.well-known/oauth-authorization-server`, opens a
 * browser tab where the user signs in via Cognito, and stores the
 * resulting access token. From there every MCP call carries that token —
 * no friction, no PAT to mint.
 *
 * No bash here: the steps are paste-this-URL + click-Authorize, not
 * shell commands.
 */
function buildConnectorOauthSteps(tool: ClientTool): InstallStep[] {
  return [
    {
      title: "Copy your MCP server URL",
      description: "This is the single URL all AI tools use. Paste it once per tool.",
      code: "<mcp-server-url>",
      langLabel: "url",
      codeSingleRowScroll: true,
    },
    {
      title: `Connect ${tool.label} to Meko`,
      description: tool.connectorInstructions,
    },
    {
      title: "Authorize",
      description:
        `Once the Meko MCP server URL is configured in ${tool.label}, authorize it to use the MCP tools. Your tool opens a browser tab, you sign in to Meko, and the token is stored automatically.`,
    },
  ];
}

function buildInstallGuide(
  mcpServerUrl: string | null | undefined,
  tool: ClientTool,
): Record<IdeTab, InstallStep[]> {
  const inject = (steps: InstallStep[]) =>
    steps.map((step) => ({
      ...step,
      code: step.code != null ? injectMcpProvisionIntoCode(step.code, mcpServerUrl) : undefined,
    }));

  // Raw (non-shell-quoted) URL substitution for the Connector (OAuth)
  // tab. The standard `inject` runs `shellSingleQuoted(url)` on every
  // URL because the other tabs' code is bash one-liners that need
  // quoting. The OAuth tab's "code" is a literal URL the user
  // copy-pastes into Cursor's MCP-URL field or Claude Desktop's
  // connector input — GUI inputs, not shells. Shell-quoting would
  // paste `'https://...'` (with surrounding single quotes) into those
  // fields, breaking the connection. We also substitute in
  // `description` here so the Claude Code `claude mcp add meko
  // <mcp-server-url> --transport http` snippet renders the real URL
  // instead of the placeholder literal — the standard inject helper
  // ignores descriptions entirely.
  const injectRaw = (steps: InstallStep[]) => {
    const url = mcpServerUrl?.trim();
    if (!url) return steps;
    return steps.map((step) => ({
      ...step,
      code: step.code?.replaceAll("<mcp-server-url>", url),
      description: step.description.replaceAll("<mcp-server-url>", url),
    }));
  };

  return {
    client_installer: inject(buildClientInstallerSteps(tool)),
    connector_oauth: injectRaw(buildConnectorOauthSteps(tool)),
    manual_mcp: inject(MANUAL_MCP_INSTALL_STEPS),
  };
}

const TAB_META: { id: IdeTab; label: string; icon: typeof Code2 }[] = [
  { id: "client_installer", label: "Client Installer", icon: Code2 },
  { id: "connector_oauth", label: "Connector (OAuth)", icon: Globe },
  { id: "manual_mcp", label: "Manual MCP Install", icon: Terminal },
];

/**
 * Icon-chip row letting the user pick which AI tool the install steps
 * should reflect. Sits at the top of the Client Installer and
 * Connector (OAuth) tabs. Selection is hoisted to `McpInstallGuide` so
 * picking a tool on one tab carries over to the other — both tabs
 * answer the same question ("which tool are you connecting?") so we
 * don't want the user to repeat the choice.
 */
function ToolPicker({
  value,
  onChange,
}: {
  value: ClientTool;
  onChange: (next: ClientTool) => void;
}) {
  return (
    <div className="mb-6 flex flex-wrap gap-2" role="radiogroup" aria-label="Choose your AI tool">
      {CLIENT_TOOLS.map((tool) => {
        const Icon = tool.Icon;
        const isActive = tool.slug === value.slug;
        return (
          <button
            key={tool.slug}
            type="button"
            role="radio"
            aria-checked={isActive}
            onClick={() => onChange(tool)}
            className={cn(
              "inline-flex items-center gap-2 rounded-lg border px-3 py-2 text-sm font-medium transition-colors",
              isActive
                ? "border-brand/50 bg-brand/10 text-brand dark:border-brand/60 dark:bg-brand/15"
                : "border-border bg-card/40 text-muted-foreground hover:border-border/80 hover:bg-card/70 hover:text-foreground",
            )}
          >
            <Icon className="h-4 w-4 shrink-0" aria-hidden />
            {tool.label}
          </button>
        );
      })}
    </div>
  );
}

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
                <h3 className="text-base font-semibold tracking-tight text-foreground">
                  {step.title}
                </h3>
                <p className="whitespace-pre-line text-sm leading-relaxed text-muted-foreground">
                  {step.description}
                </p>
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
  // Selected tool is shared across the Client Installer and Connector
  // (OAuth) tabs — the user is answering the same question ("which
  // tool?") on both surfaces, so we lift state up here and pass the
  // resolved tool down to whichever tab is active.
  const [selectedTool, setSelectedTool] = useState<ClientTool>(DEFAULT_CLIENT_TOOL);

  const installGuide = useMemo(
    () => buildInstallGuide(mcpServerUrl, selectedTool),
    [mcpServerUrl, selectedTool],
  );

  // Tabs that show the ToolPicker. Manual MCP Install is intentionally
  // excluded — that flow targets generic MCP clients (`npx mcp-add`)
  // and doesn't have a `--client` slug per tool.
  const tabsWithPicker = new Set<IdeTab>(["client_installer", "connector_oauth"]);

  return (
    <Tabs defaultValue="client_installer" className={cn("w-full", className)}>
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
        <TabsContent
          key={id}
          value={id}
          className="mt-6 rounded-xl border border-border bg-card/30 p-6 sm:p-8"
        >
          {tabsWithPicker.has(id) ? (
            <ToolPicker value={selectedTool} onChange={setSelectedTool} />
          ) : null}
          <InstallSteps tab={id} steps={installGuide[id]} />
        </TabsContent>
      ))}
    </Tabs>
  );
}
