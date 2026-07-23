// Connect-to-datapack dialog.
//
// Surfaces the MCP server URL + the user's Meko personal access tokens
// (NOT the legacy datapack-scoped mcp_api_key). Tokens are fetched lazily
// when the dialog opens via GET /me/meko-tokens, and only active ones
// are shown — expired / revoked rows live in Settings → Security.
//
// When the user has no active tokens, the body collapses to an empty
// state that links to /settings (closes the dialog on click — the user
// pressed Connect, an auto-redirect would feel like a bug).

import { useCallback, useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Check, Copy, Link as LinkIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useTruncatableCodeDisplay } from "@/hooks/useTruncatableCodeDisplay";
import { copyToClipboard } from "@/lib/clipboard";
import { cn } from "@/lib/utils";
import { buildMekoInstallerCommand } from "@/lib/mekoInstallerCommand";
import {
  getApiErrorMessage,
  listMekoTokens,
  type MekoToken,
  type ProvisionStatus,
} from "@/lib/api/mekoApi";
import {
  MEKO_TOKEN_INSTALLER_PLACEHOLDER,
  filterActiveMekoTokens,
} from "@/lib/mekoTokens";
import { ROUTES } from "@/app/routes/paths";
import { ConnectApiTokensField } from "./ConnectApiTokensField";

/** Collapsed one-line installer preview; copy always uses the full command. */
const INSTALLER_COMMAND_PREVIEW_MAX_CHARS = 320;

function InstallerCommandRow({
  command,
  copyField,
  copiedField,
  onCopy,
  dialogOpen,
}: {
  command: string;
  copyField: string;
  copiedField: string | null;
  onCopy: (text: string, field: string) => void;
  dialogOpen: boolean;
}) {
  const { displayText, needsToggle, expanded, toggleExpanded } = useTruncatableCodeDisplay(
    command,
    INSTALLER_COMMAND_PREVIEW_MAX_CHARS,
    `${dialogOpen ? "1" : "0"}:${command}`,
  );

  return (
    <div className="flex items-start gap-2 rounded-md border border-border bg-card p-3">
      <div className="flex min-w-0 flex-1 flex-col gap-2">
        <code className="min-w-0 break-all text-xs leading-relaxed text-blue-700 dark:text-blue-100">{displayText}</code>
        {needsToggle ? (
          <button
            type="button"
            className="self-start text-sm font-medium text-blue-400 hover:text-blue-700 dark:text-blue-300"
            onClick={toggleExpanded}
          >
            {expanded ? "Show less" : "Show more"}
          </button>
        ) : null}
      </div>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        className="h-8 w-8 shrink-0 text-muted-foreground hover:text-white"
        onClick={() => void onCopy(command, copyField)}
      >
        {copiedField === copyField ? (
          <Check className="h-4 w-4 text-green-500" />
        ) : (
          <Copy className="h-4 w-4" />
        )}
      </Button>
    </div>
  );
}

export type DatapackConnectDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  error: string | null;
  provisionStatus: ProvisionStatus | null;
  /**
   * Whether to render the "Datapack: <name>" row at the top of the credentials
   * section. Default `true` — matches the per-datapack entry points (the
   * Connect tile on the detail page) where the name reinforces context.
   *
   * Set to `false` when the dialog is opened from a *cross-datapack* surface
   * (e.g. the high-level Connect CTA on the Datapacks Overview page). There,
   * "Connect" is a Meko-level action: we still need a real provisioned
   * datapack under the hood to mint the credentials, but exposing its name
   * would be misleading because the user didn't pick it — we did.
   */
  showDatapackName?: boolean;
};

export function DatapackConnectDialog({
  open,
  onOpenChange,
  loading,
  error,
  provisionStatus,
  showDatapackName = true,
}: DatapackConnectDialogProps) {
  const navigate = useNavigate();
  const [copiedField, setCopiedField] = useState<string | null>(null);
  const [tokens, setTokens] = useState<MekoToken[]>([]);
  const [tokensLoading, setTokensLoading] = useState(false);
  const [tokensError, setTokensError] = useState<string | null>(null);

  // Lazy-fetch on open so the dialog isn't an extra page-load API call.
  // Refetches on every open so a token created in another tab shows up
  // here without a full reload.
  useEffect(() => {
    if (!open) return;
    const ac = new AbortController();
    setTokensLoading(true);
    setTokensError(null);
    void listMekoTokens({ signal: ac.signal })
      .then((list) => {
        if (ac.signal.aborted) return;
        setTokens(list ?? []);
      })
      .catch((err) => {
        if (ac.signal.aborted) return;
        if ((err as { name?: string })?.name === "CanceledError") return;
        setTokensError(getApiErrorMessage(err, "Failed to load API keys."));
        setTokens([]);
      })
      .finally(() => {
        if (!ac.signal.aborted) setTokensLoading(false);
      });
    return () => ac.abort();
  }, [open]);

  const handleCopy = useCallback(async (text: string, field: string) => {
    const copied = await copyToClipboard(text);
    if (!copied) return;
    setCopiedField(field);
    window.setTimeout(() => setCopiedField(null), 2000);
  }, []);

  const goToSettings = useCallback(() => {
    onOpenChange(false);
    navigate(ROUTES.settings);
  }, [navigate, onOpenChange]);

  const activeTokens = filterActiveMekoTokens(tokens);

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        onOpenChange(next);
        if (!next) {
          setCopiedField(null);
        }
      }}
    >
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-xl max-h-[calc(100vh-4rem)] bg-card border-border text-foreground p-0 flex flex-col">
        <DialogHeader className="px-6 pt-6 pb-3 shrink-0">
          <div className="flex items-center gap-2">
            <LinkIcon className="h-5 w-5 shrink-0 text-blue-400" aria-hidden />
            <DialogTitle className="text-xl text-foreground">Connect to Datapack</DialogTitle>
          </div>
          <DialogDescription className="text-muted-foreground">
            Use these credentials to connect your MCP server and database.
          </DialogDescription>
        </DialogHeader>

        {loading ? (
          <div className="px-6 pb-6 py-8 text-sm text-muted-foreground text-center overflow-y-auto">
            Loading connection details...
          </div>
        ) : error ? (
          <div className="px-6 pb-6 py-6 overflow-y-auto">
            <p className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-700 dark:text-red-200">
              {error}
            </p>
          </div>
        ) : provisionStatus ? (
          <div className="space-y-4 px-6 pb-6 py-4 overflow-y-auto min-h-0">
            {showDatapackName ? (
              <>
                <h3 className="text-lg font-semibold text-foreground">Datapack</h3>
                <div className="space-y-2">
                  <label className="text-sm font-medium text-muted-foreground">Datapack</label>
                  <div className="flex items-center p-3 rounded-md bg-input border border-border">
                    <code className="text-sm text-foreground break-all flex-1 pr-4">
                      {provisionStatus.datapack_name}
                    </code>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-8 w-8 text-muted-foreground hover:text-white"
                      onClick={() => void handleCopy(provisionStatus.datapack_name, "dp")}
                    >
                      {copiedField === "dp" ? (
                        <Check className="h-4 w-4 text-green-500" />
                      ) : (
                        <Copy className="h-4 w-4" />
                      )}
                    </Button>
                  </div>
                </div>
              </>
            ) : null}

            <h3 className={cn("text-lg font-semibold text-foreground", showDatapackName && "pt-2")}>MCP Server</h3>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">URL</label>
              <div className="flex items-center p-3 rounded-md bg-input border border-border">
                <code className="text-xs text-foreground break-all flex-1 pr-4">
                  {provisionStatus.mcp_server_url}
                </code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => void handleCopy(provisionStatus.mcp_server_url, "mcpurl")}
                >
                  {copiedField === "mcpurl" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>

            <ConnectApiTokensField
              loading={tokensLoading}
              error={tokensError}
              tokens={activeTokens}
              totalCount={tokens.length}
              copiedField={copiedField}
              onCopy={handleCopy}
              onOpenSettings={goToSettings}
            />

            <div className="space-y-2 pt-1">
              <label className="text-sm font-medium text-muted-foreground">One-line installer</label>
              <p className="text-xs leading-relaxed text-muted-foreground">
                Replace{" "}
                <code className="rounded bg-input px-1 text-foreground/90">
                  {MEKO_TOKEN_INSTALLER_PLACEHOLDER}
                </code>{" "}
                in the command with the API key you saved when generating it.
              </p>
              <Tabs defaultValue="claude-code" className="w-full">
                <TabsList className="h-9 w-full justify-start bg-input p-1 border border-border">
                  <TabsTrigger
                    value="claude-code"
                    className="flex-1 data-[state=active]:bg-blue-600 data-[state=active]:text-white data-[state=inactive]:text-muted-foreground"
                  >
                    Claude Code
                  </TabsTrigger>
                  <TabsTrigger
                    value="claude-desktop"
                    className="flex-1 data-[state=active]:bg-blue-600 data-[state=active]:text-white data-[state=inactive]:text-muted-foreground"
                  >
                    Claude Desktop
                  </TabsTrigger>
                  <TabsTrigger
                    value="cursor"
                    className="flex-1 data-[state=active]:bg-blue-600 data-[state=active]:text-white data-[state=inactive]:text-muted-foreground"
                  >
                    Cursor
                  </TabsTrigger>
                  <TabsTrigger
                    value="codex"
                    className="flex-1 data-[state=active]:bg-blue-600 data-[state=active]:text-white data-[state=inactive]:text-muted-foreground"
                  >
                    Codex
                  </TabsTrigger>
                </TabsList>
                <TabsContent value="claude-desktop" className="mt-3 space-y-2 outline-none">
                  <InstallerCommandRow
                    dialogOpen={open}
                    command={buildMekoInstallerCommand("claude-desktop", provisionStatus.mcp_server_url)}
                    copyField="mcpInstallClaudeDesktop"
                    copiedField={copiedField}
                    onCopy={handleCopy}
                  />
                </TabsContent>
                <TabsContent value="claude-code" className="mt-3 space-y-2 outline-none">
                  <InstallerCommandRow
                    dialogOpen={open}
                    command={buildMekoInstallerCommand("claude-code", provisionStatus.mcp_server_url)}
                    copyField="mcpInstallClaudeCode"
                    copiedField={copiedField}
                    onCopy={handleCopy}
                  />
                </TabsContent>
                <TabsContent value="cursor" className="mt-3 space-y-2 outline-none">
                  <InstallerCommandRow
                    dialogOpen={open}
                    command={buildMekoInstallerCommand("cursor", provisionStatus.mcp_server_url)}
                    copyField="mcpInstallCursor"
                    copiedField={copiedField}
                    onCopy={handleCopy}
                  />
                </TabsContent>
                <TabsContent value="codex" className="mt-3 space-y-2 outline-none">
                  <InstallerCommandRow
                    dialogOpen={open}
                    command={buildMekoInstallerCommand("codex", provisionStatus.mcp_server_url)}
                    copyField="mcpInstallCodex"
                    copiedField={copiedField}
                    onCopy={handleCopy}
                  />
                </TabsContent>
              </Tabs>
            </div>
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
