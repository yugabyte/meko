import { useState } from "react";
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
import { copyToClipboard } from "@/lib/clipboard";
import { buildMekoInstallerCommand } from "@/lib/mekoInstallerCommand";
import type { ProvisionStatus } from "@/lib/mekoApi";

export type DatapackConnectDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  error: string | null;
  provisionStatus: ProvisionStatus | null;
};

export function DatapackConnectDialog({
  open,
  onOpenChange,
  loading,
  error,
  provisionStatus,
}: DatapackConnectDialogProps) {
  const [copiedField, setCopiedField] = useState<string | null>(null);

  const handleCopy = async (text: string, field: string) => {
    const copied = await copyToClipboard(text);
    if (!copied) return;
    setCopiedField(field);
    window.setTimeout(() => setCopiedField(null), 2000);
  };

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
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-xl max-h-[calc(100vh-4rem)] bg-[#1C2333] border-border text-foreground p-0 flex flex-col">
        <DialogHeader className="px-6 pt-6 pb-3 shrink-0">
          <div className="flex items-center gap-2">
            <LinkIcon className="h-5 w-5 shrink-0 text-blue-400" aria-hidden />
            <DialogTitle className="text-xl text-white">Connect to Datapack</DialogTitle>
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
            <p className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">
              {error}
            </p>
          </div>
        ) : provisionStatus ? (
          <div className="space-y-4 px-6 pb-6 py-4 overflow-y-auto min-h-0">
            <h3 className="text-lg font-semibold text-white">Datapack</h3>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">Datapack</label>
              <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                <code className="text-sm text-white break-all flex-1 pr-4">
                  {provisionStatus.datapack_name}
                </code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => handleCopy(provisionStatus.datapack_name, "dp")}
                >
                  {copiedField === "dp" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>

            <h3 className="pt-2 text-lg font-semibold text-white">MCP Server</h3>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">URL</label>
              <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                <code className="text-xs text-white break-all flex-1 pr-4">
                  {provisionStatus.mcp_server_url}
                </code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => handleCopy(provisionStatus.mcp_server_url, "mcpurl")}
                >
                  {copiedField === "mcpurl" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">API Key</label>
              <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.mcp_api_key}</code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => handleCopy(provisionStatus.mcp_api_key, "mcpkey")}
                >
                  {copiedField === "mcpkey" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>

            <div className="space-y-2 pt-1">
              <label className="text-sm font-medium text-muted-foreground">One-line installer</label>
              <p className="text-xs leading-relaxed text-muted-foreground">
                Run from the directory that contains the <code className="text-muted-foreground/90">installer</code>{" "}
                folder (see product docs). Values below use this datapack&apos;s MCP URL and API key.
              </p>
              <Tabs defaultValue="claude-code" className="w-full">
                <TabsList className="h-9 w-full justify-start bg-[#0E1525] p-1 border border-border">
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
                </TabsList>
                <TabsContent value="claude-desktop" className="mt-3 space-y-2 outline-none">
                  <div className="flex items-start gap-2 rounded-md border border-border bg-[#0E1525] p-3">
                    <code className="min-w-0 flex-1 break-all text-xs leading-relaxed text-blue-100">
                      {buildMekoInstallerCommand(
                        "claude-desktop",
                        provisionStatus.mcp_server_url,
                        provisionStatus.mcp_api_key,
                      )}
                    </code>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-8 w-8 shrink-0 text-muted-foreground hover:text-white"
                      onClick={() =>
                        handleCopy(
                          buildMekoInstallerCommand(
                            "claude-desktop",
                            provisionStatus.mcp_server_url,
                            provisionStatus.mcp_api_key,
                          ),
                          "mcpInstallClaude",
                        )
                      }
                    >
                      {copiedField === "mcpInstallClaude" ? (
                        <Check className="h-4 w-4 text-green-500" />
                      ) : (
                        <Copy className="h-4 w-4" />
                      )}
                    </Button>
                  </div>
                </TabsContent>
                <TabsContent value="claude-code" className="mt-3 space-y-2 outline-none">
                  <div className="flex items-start gap-2 rounded-md border border-border bg-[#0E1525] p-3">
                    <code className="min-w-0 flex-1 break-all text-xs leading-relaxed text-blue-100">
                      {buildMekoInstallerCommand(
                        "claude",
                        provisionStatus.mcp_server_url,
                        provisionStatus.mcp_api_key,
                      )}
                    </code>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-8 w-8 shrink-0 text-muted-foreground hover:text-white"
                      onClick={() =>
                        handleCopy(
                          buildMekoInstallerCommand(
                            "claude",
                            provisionStatus.mcp_server_url,
                            provisionStatus.mcp_api_key,
                          ),
                          "mcpInstallClaude",
                        )
                      }
                    >
                      {copiedField === "mcpInstallClaude" ? (
                        <Check className="h-4 w-4 text-green-500" />
                      ) : (
                        <Copy className="h-4 w-4" />
                      )}
                    </Button>
                  </div>
                </TabsContent>
                <TabsContent value="cursor" className="mt-3 space-y-2 outline-none">
                  <div className="flex items-start gap-2 rounded-md border border-border bg-[#0E1525] p-3">
                    <code className="min-w-0 flex-1 break-all text-xs leading-relaxed text-blue-100">
                      {buildMekoInstallerCommand(
                        "cursor",
                        provisionStatus.mcp_server_url,
                        provisionStatus.mcp_api_key,
                      )}
                    </code>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="h-8 w-8 shrink-0 text-muted-foreground hover:text-white"
                      onClick={() =>
                        handleCopy(
                          buildMekoInstallerCommand(
                            "cursor",
                            provisionStatus.mcp_server_url,
                            provisionStatus.mcp_api_key,
                          ),
                          "mcpInstallCursor",
                        )
                      }
                    >
                      {copiedField === "mcpInstallCursor" ? (
                        <Check className="h-4 w-4 text-green-500" />
                      ) : (
                        <Copy className="h-4 w-4" />
                      )}
                    </Button>
                  </div>
                </TabsContent>
              </Tabs>
            </div>

            <h3 className="pt-2 text-lg font-semibold text-white">Database Access</h3>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">Username</label>
              <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.db_user_name}</code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => handleCopy(provisionStatus.db_user_name, "dbuser")}
                >
                  {copiedField === "dbuser" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">Password</label>
              <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.db_password}</code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => handleCopy(provisionStatus.db_password, "dbpass")}
                >
                  {copiedField === "dbpass" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>
            <div className="space-y-2">
              <label className="text-sm font-medium text-muted-foreground">Conn String</label>
              <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                <code className="text-xs text-blue-300 break-all flex-1 pr-4">{provisionStatus.db_conn_str}</code>
                <Button
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 text-muted-foreground hover:text-white"
                  onClick={() => handleCopy(provisionStatus.db_conn_str, "dbconn")}
                >
                  {copiedField === "dbconn" ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </div>
            </div>
          </div>
        ) : null}
      </DialogContent>
    </Dialog>
  );
}
