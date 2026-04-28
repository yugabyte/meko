import { useState } from "react";
import { Check, Copy, Link as LinkIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";

function buildMekoSetupCommand(mcpServerUrl: string, mcpApiKey: string): string {
  return `node installer/bin/create-meko-setup.mjs --client <client-name> --url ${(mcpServerUrl)} --api-key <mcp-api-key>`;
}

type Props = {
  mcpServerUrl: string | null;
  mcpApiKey: string | null;
  onOpenConnect: () => void;
  className?: string;
};

/**
 * Sidebar tile: one-line installer command using MCP URL + API key from provision status (same source as Connect dialog).
 */
export function DatapackDetailConnectTile({
  mcpServerUrl,
  mcpApiKey,
  onOpenConnect,
  className,
}: Props) {
  const [copied, setCopied] = useState(false);
  const ready = Boolean(mcpServerUrl && mcpApiKey);
  const command = ready ? buildMekoSetupCommand(mcpServerUrl!, mcpApiKey!) : "";

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
        "flex h-fit min-h-0 w-full flex-col self-start rounded-xl border border-border bg-card/50 shadow-sm",
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
            <div className="relative rounded-md border border-border bg-[#0E1525] p-3 pr-11">
              <pre className="whitespace-pre-wrap break-all font-mono text-[11px] leading-relaxed text-foreground/95">
                {command}
              </pre>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="absolute right-1 top-1 h-8 w-8 text-muted-foreground hover:text-foreground"
                onClick={() => void handleCopy()}
                aria-label="Copy setup command"
              >
                {copied ? <Check className="h-4 w-4 text-green-500" aria-hidden /> : <Copy className="h-4 w-4" aria-hidden />}
              </Button>
            </div>
            <p className="text-[11px] leading-snug text-muted-foreground">
              Uses the same MCP server URL and API key as{' '}
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
            Open{' '}
            <button
              type="button"
              className="font-medium text-blue-400 underline underline-offset-2 hover:text-blue-300"
              onClick={onOpenConnect}
            >
              Connect
            </button>{' '}
            to load your MCP URL and API key; the command will appear here.
          </p>
        )}
      </div>
    </aside>
  );
}
