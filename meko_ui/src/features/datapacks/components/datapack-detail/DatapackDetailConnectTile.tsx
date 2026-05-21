import { useState } from "react";
import { Check, Copy, Link as LinkIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";
import { MEKO_TOKEN_INSTALLER_PLACEHOLDER } from "@/lib/mekoTokens";

function buildMekoSetupCommand(mcpServerUrl: string): string {
  return `npx @yugabytedb/meko-mcp --client <client-name> --url ${mcpServerUrl} --api-key ${MEKO_TOKEN_INSTALLER_PLACEHOLDER}`;
}

type Props = {
  mcpServerUrl: string | null;
  onOpenConnect: () => void;
  className?: string;
};

/**
 * Sidebar tile: one-line installer command (truncated + copy). The
 * command uses the {@link MEKO_TOKEN_INSTALLER_PLACEHOLDER} placeholder
 * for the API key — the user copies the actual token from Settings →
 * Security (also linked from the Connect dialog).
 */
export function DatapackDetailConnectTile({
  mcpServerUrl,
  onOpenConnect,
  className,
}: Props) {
  const [copied, setCopied] = useState(false);
  const ready = Boolean(mcpServerUrl);
  const command = ready ? buildMekoSetupCommand(mcpServerUrl!) : "";

  const handleCopy = async () => {
    if (!ready) return;
    const ok = await copyToClipboard(command);
    if (!ok) return;
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  };

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 w-full flex-col rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <LinkIcon className="h-4 w-4 shrink-0 text-blue-400" aria-hidden />
        <span className="text-sm font-semibold text-foreground">Connect</span>
      </div>
      <div className="flex flex-col gap-3 px-3 py-3">
        {ready ? (
          <>
            <div className="relative flex min-h-[2.5rem] items-center rounded-md border border-border bg-[#0E1525] py-1.5 pl-3 pr-10">
              <p
                className="min-w-0 flex-1 truncate font-mono text-[11px] leading-snug text-foreground/95"
                title={command}
              >
                {command}
              </p>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="absolute right-1 top-1/2 h-8 w-8 -translate-y-1/2 shrink-0 text-muted-foreground hover:text-foreground"
                onClick={() => void handleCopy()}
                aria-label="Copy setup command"
              >
                {copied ? <Check className="h-4 w-4 text-green-500" aria-hidden /> : <Copy className="h-4 w-4" aria-hidden />}
              </Button>
            </div>
            <p className="text-[11px] leading-snug text-muted-foreground">
              Replace{" "}
              <code className="font-mono text-foreground/90">{MEKO_TOKEN_INSTALLER_PLACEHOLDER}</code>{" "}
              with a Meko API key from{" "}
              <button
                type="button"
                className="font-medium text-blue-400 underline underline-offset-2 hover:text-blue-300"
                onClick={onOpenConnect}
              >
                Connect
              </button>
              .
            </p>
          </>
        ) : (
          <p className="text-xs leading-relaxed text-muted-foreground">
            Open{" "}
            <button
              type="button"
              className="font-medium text-blue-400 underline underline-offset-2 hover:text-blue-300"
              onClick={onOpenConnect}
            >
              Connect
            </button>{" "}
            to load your MCP URL; the command will appear here.
          </p>
        )}
      </div>
    </aside>
  );
}
