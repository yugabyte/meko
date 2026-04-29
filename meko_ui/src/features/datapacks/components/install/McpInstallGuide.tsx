/**
 * MCP client install guide: tabs for Claude Code, Claude Desktop, and Cursor with copyable shell steps.
 * Shared by the Home “Install” page and the datapack configure wizard’s Install step.
 */
import { useState } from "react";
import { Check, Copy, Code2, Monitor, MousePointer2 } from "lucide-react";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";

type IdeTab = "claude_code" | "claude_desktop" | "cursor";

type InstallStep = {
  title: string;
  description: string;
  code?: string;
  langLabel?: string;
};

const CLAUDE_CODE_INSTALL_STEP: InstallStep = {
  title: "Claude Code",
  description:
    "Run the installer from the directory that contains it. This configures the MCP server connection for Claude Code, sets environment variables, and installs the skills plugin into the agent.",
  code: 'node installer/bin/create-meko-setup.mjs --client claude-code --url <meko-mcp-url> --api-key "<meko-server-api-key>"',
  langLabel: "bash",
};

const CLAUDE_DESKTOP_INSTALL_STEP: InstallStep = {
  title: "Claude Desktop",
  description:
    "If you use the Claude desktop app instead of Claude Code, use this command with your MCP URL and API key from the Datapacks page (same installer path as above).",
  code: 'node installer/bin/create-meko-setup.mjs --client claude-desktop --url <mcp-server-url> --api-key "<mcp-server-api-key>"',
  langLabel: "bash",
};

const CURSOR_INSTALL_STEP: InstallStep = {
  title: "Run the Cursor installer",
  description:
    "One command registers the MCP server with Cursor using your MCP URL and API key from the Datapacks page. Replace the placeholders, then run from the directory that contains the installer (same layout as the Claude tabs).",
  code: 'node installer/bin/create-meko-setup.mjs --client cursor --url <mcp-server-url> --api-key "<mcp-server-api-key>"',
  langLabel: "bash",
};

const INSTALL_GUIDE: Record<IdeTab, InstallStep[]> = {
  claude_code: [CLAUDE_CODE_INSTALL_STEP],
  claude_desktop: [CLAUDE_DESKTOP_INSTALL_STEP],
  cursor: [CURSOR_INSTALL_STEP],
};

const TAB_META: { id: IdeTab; label: string; icon: typeof Code2 }[] = [
  { id: "claude_code", label: "Claude Code", icon: Code2 },
  { id: "claude_desktop", label: "Claude Desktop", icon: Monitor },
  { id: "cursor", label: "Cursor", icon: MousePointer2 },
];

function CodeBlock({
  code,
  langLabel,
  copyId,
  copiedId,
  onCopy,
}: {
  code: string;
  langLabel: string;
  copyId: string;
  copiedId: string | null;
  onCopy: (text: string, id: string) => void;
}) {
  return (
    <div className="max-w-full overflow-hidden rounded-lg border border-border bg-[#0E1525]/80 shadow-sm">
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
      <pre className="max-w-full p-4 text-sm leading-relaxed text-foreground/95">
        <code className="block whitespace-pre-wrap break-words font-mono [overflow-wrap:anywhere]">{code}</code>
      </pre>
    </div>
  );
}

function InstallSteps({ tab }: { tab: IdeTab }) {
  const steps = INSTALL_GUIDE[tab];
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
};

/**
 * MCP install steps (Claude Code / Claude Desktop / Cursor).
 */
export function McpInstallGuide({ className }: McpInstallGuideProps) {
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
          <InstallSteps tab={id} />
        </TabsContent>
      ))}
    </Tabs>
  );
}
