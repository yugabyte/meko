import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import type { LucideIcon } from "lucide-react";
import {
  ArrowLeftRight,
  BookOpen,
  BrainCircuit,
  Check,
  Copy,
  Home,
  Sparkles,
  SquareTerminal,
  MousePointer2,
} from "lucide-react";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { getApiErrorMessage, listDatapacks } from "@/lib/mekoApi";
import { copyToClipboard } from "@/lib/clipboard";
import { ROUTES } from "@/app/routes/paths";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

type IdeTab = "claude" | "codex" | "cursor";

type InstallStep = {
  title: string;
  description: string;
  /** When omitted or empty, no code block is shown (e.g. interactive-only steps). */
  code?: string;
  langLabel?: string;
};

const NVM_INSTALL_STEP: InstallStep = {
  title: "Install NVM",
  description:
    "Install Node Version Manager so you can run the tooling used to register the MCP server with your assistant.",
  code: "curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash",
  langLabel: "bash",
};

const NVM_SOURCE_STEP: InstallStep = {
  title: "Load NVM in your shell",
  description:
    "Source your shell configuration so nvm is available in the current session (adjust the file if you use a custom profile).",
  code: "source ~/.$(basename $SHELL)rc",
  langLabel: "bash",
};

function mcpIntegrateStep(productLabel: string, clientsArg: string): InstallStep {
  const code = `npx mcp-add@latest \\
  --name meko \\
  --type stdio \\
  --command 'npx -y mcp-remote <meko-mcp-url> --allow-http' \\
  --env 'MEKO_API_KEY=<meko-mcp-api-key>' \\
  --headers 'MEKO_API_KEY=<meko-api-key>' \\
  --scope global \\
  --clients '${clientsArg}'`;

  return {
    title: `Integrate MCP server into ${productLabel}`,
    description:
      "Run mcp-add with your meko MCP URL and API keys from the Datapacks page. Replace placeholders before executing.",
    code,
    langLabel: "bash",
  };
}

const CLAUDE_CODE_INSTALL_STEP: InstallStep = {
  title: "Run the Claude Code installer",
  description:
    "This installer configures the MCP server connection, sets environment variables, and installs the skills plugin into the AI agent.",
  code: 'node installer/bin/create-meko-setup.mjs --url <meko-mcp-url> --api-key "<meko-api-key>" --yes',
  langLabel: "bash",
};

const SKILLS_PACKAGE_STEP: InstallStep = {
  title: "Integrate skills package with the AI agent",
  description:
    "Integrate skills package to AI agent to help learn how to use Meko's MCP tools for memory, knowledge.",
  code: `git clone https://github.com/yugabyte/meko-mcp-server.git
cd meko-mcp-server
git checkout add-skills
npx skills add ./skills -g`,
  langLabel: "bash",
};

const SKILLS_PROMPT_STEP: InstallStep = {
  title: "Select skills at the prompt",
  description:
    "When prompted to select the skills to install, select both meko-mcp-tools and ysql.",
};

const CODEX_CURSOR_EXTRA_STEPS: InstallStep[] = [SKILLS_PACKAGE_STEP, SKILLS_PROMPT_STEP];

const INSTALL_GUIDE: Record<IdeTab, InstallStep[]> = {
  claude: [CLAUDE_CODE_INSTALL_STEP],
  codex: [
    NVM_INSTALL_STEP,
    NVM_SOURCE_STEP,
    mcpIntegrateStep("Codex", "codex"),
    ...CODEX_CURSOR_EXTRA_STEPS,
  ],
  cursor: [
    NVM_INSTALL_STEP,
    NVM_SOURCE_STEP,
    mcpIntegrateStep("Cursor", "cursor"),
    ...CODEX_CURSOR_EXTRA_STEPS,
  ],
};

const TAB_META: { id: IdeTab; label: string; icon: typeof Sparkles }[] = [
  { id: "claude", label: "Claude Code", icon: Sparkles },
  { id: "codex", label: "OpenAI Codex", icon: SquareTerminal },
  { id: "cursor", label: "Cursor", icon: MousePointer2 },
];

type HomeStatCard = {
  title: string;
  value: number;
  countLabel?: string;
  icon: LucideIcon;
  iconClass: string;
};

const HOME_STAT_CARDS: HomeStatCard[] = [
  {
    title: "Memory (ME)",
    value: 0, // TODO: Implement a memory count API which will contain objects of datapack name and its memory count
    countLabel: "conversations",
    icon: BrainCircuit,
    iconClass: "text-violet-400",
  },
  {
    title: "Knowledge (KO)",
    value: 0, // TODO: Implement a count of Knowledge Base API which will contain objects of datapack name and its KB count
    countLabel: "files",
    icon: BookOpen,
    iconClass: "text-emerald-400",
  },
  {
    title: "Learning (ME → KO)",
    value: 0, // TODO: Implement a count of promoted Mem to Knowledge API which will contain objects of datapack name and its learning count
    icon: ArrowLeftRight,
    iconClass: "text-amber-400",
  },
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
    <div className="rounded-lg border border-border overflow-hidden bg-[#0E1525]/80 shadow-sm">
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
      <pre className="overflow-x-auto p-4 text-sm leading-relaxed text-foreground/95">
        <code>{code}</code>
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
        className="pointer-events-none absolute left-[17px] top-5 bottom-5 hidden w-px bg-border lg:block"
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
              <div className="space-y-2 pt-0.5 min-w-0">
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

export default function HomePage() {
  const [count, setCount] = useState<number | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      setLoading(true);
      setError(null);
      try {
        const items = await listDatapacks();
        if (!cancelled) setCount(items.length);
      } catch (e) {
        if (!cancelled) {
          setError(getApiErrorMessage(e, "Failed to load datapacks."));
          setCount(null);
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    void run();
    return () => {
      cancelled = true;
    };
  }, []);

  const n = count ?? 0;
  const datapackWord = n === 1 ? "datapack" : "datapacks";

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader showCliButton={false} />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-10 pb-16">
        <div className="flex flex-col gap-2">
          <div className="flex items-center gap-3">
            <Home className="w-6 h-6 text-blue-400" />
            <h1 className="text-2xl font-semibold tracking-tight">Home</h1>
          </div>
        </div>

        <div className="grid gap-4 sm:grid-cols-3">
          {HOME_STAT_CARDS.map(({ title, value, countLabel, icon: Icon, iconClass }) => (
            <Card key={title} className="border-border bg-card/50 shadow-sm">
              <CardContent className="p-6 flex flex-col gap-1">
                <div className="flex items-center gap-2 text-muted-foreground text-sm font-medium">
                  <Icon className={cn("h-4 w-4 shrink-0", iconClass)} aria-hidden />
                  <span>{title}</span>
                </div>
                <div className="mt-2 flex items-baseline gap-2 items-center">
                  <span className="text-4xl font-semibold tabular-nums tracking-tight">{value}</span>
                  {countLabel ? (
                    <span className="text-sm text-muted-foreground">{countLabel}</span>
                  ) : null}
                </div>
              </CardContent>
            </Card>
          ))}
        </div>

        {!loading && !error ? (
          <p className="text-sm leading-relaxed text-muted-foreground">
            Your workspace has <span className="font-medium text-foreground">{n}</span>{" "}
            {datapackWord}.{" "}
            <Link
              to={ROUTES.datapacks}
              className="font-medium text-blue-400 underline-offset-4 hover:underline"
            >
              View Datapacks
            </Link>{" "}
          </p>
        ) : loading ? (
          <p className="text-sm text-muted-foreground">Loading datapack summary…</p>
        ) : null}

        <section className="space-y-6 pt-2">
          <div>
            <h2 className="text-xl font-semibold tracking-tight">Install meko</h2>
            <p className="mt-1 text-sm text-muted-foreground max-w-2xl">
              Choose your environment and follow the steps.
            </p>
          </div>

          <Tabs defaultValue="claude" className="w-full">
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
              <TabsContent key={id} value={id} className="mt-8 rounded-xl border border-border bg-card/30 p-6 sm:p-8">
                <InstallSteps tab={id} />
              </TabsContent>
            ))}
          </Tabs>
        </section>
      </main>
    </div>
  );
}
