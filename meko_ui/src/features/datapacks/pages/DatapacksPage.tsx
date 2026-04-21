import { useEffect, useMemo, useState } from "react";
import { Plus, Database, Terminal, Users, BrainCircuit, Library, ArrowUpDown, Link as LinkIcon, Copy, Check, Sparkles, Book, ChevronDown, ChevronUp } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import {
  createDatapack,
  getProvisionStatus,
  listAgents,
  listDatapacks,
  listKnowledgeBases,
  getDatapackStatus,
  ensureLangfuseProjectKeys,
  type Agent,
  type Datapack,
  type DatapackStatus,
  type KnowledgeBase,
  PROVISION_STATUS_CACHE_KEY,
  type ProvisionStatus,
} from "@/lib/mekoApi";
import { copyToClipboard } from "@/lib/clipboard";

const MEMORIES = [
  { id: "m_1", agentId: "agt_9a8b7c", context: "User prefers dark mode for all dashboards", createdAt: "2023-10-26T10:00:00Z" },
  { id: "m_2", agentId: "agt_3f2e1d", context: "Asked about weather in SF", createdAt: "2023-10-26T14:30:00Z" },
  { id: "m_3", agentId: "agt_5x6y7z", context: "Analyzed Q3 report and found 15% growth", createdAt: "2023-10-25T09:15:00Z" },
  { id: "m_4", agentId: "agt_9a8b7c", context: "Remember meeting at 5pm with marketing", createdAt: "2023-10-24T16:00:00Z" },
  { id: "m_5", agentId: "agt_3f2e1d", context: "Set alarm for 7am on weekdays", createdAt: "2023-10-24T20:00:00Z" },
];

type ProvisionStatusCache = Record<string, ProvisionStatus>;
const LANGFUSE_PROJECT_KEYS_CACHE_KEY = "meko:langfuse-project-keys-cache:v1";
const LANGFUSE_PROJECT_KEYS_TTL_MS = 5 * 60_000;

function isFresh(updatedAt: number, ttlMs: number) {
  return Date.now() - updatedAt <= ttlMs;
}

function readLangfuseProjectKeysCache(): Record<string, number> {
  const raw = sessionStorage.getItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY);
  if (!raw) return {};
  try {
    return JSON.parse(raw) as Record<string, number>;
  } catch {
    sessionStorage.removeItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY);
    return {};
  }
}

function writeLangfuseProjectKeysCache(cache: Record<string, number>) {
  sessionStorage.setItem(LANGFUSE_PROJECT_KEYS_CACHE_KEY, JSON.stringify(cache));
}

function readProvisionStatusCache(): ProvisionStatusCache {
  const raw = sessionStorage.getItem(PROVISION_STATUS_CACHE_KEY);
  if (!raw) return {};
  try {
    return JSON.parse(raw) as ProvisionStatusCache;
  } catch {
    sessionStorage.removeItem(PROVISION_STATUS_CACHE_KEY);
    return {};
  }
}

function writeProvisionStatusCache(cache: ProvisionStatusCache) {
  sessionStorage.setItem(PROVISION_STATUS_CACHE_KEY, JSON.stringify(cache));
}

export default function DatapacksPage() {
  const [datapacks, setDatapacks] = useState<Datapack[]>([]);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [knowledgeBases, setKnowledgeBases] = useState<KnowledgeBase[]>([]);
  const [datapackStatus, setDatapackStatus] = useState<DatapackStatus | null>(null);
  const [activeDatapack, setActiveDatapack] = useState("");
  const [isLoading, setIsLoading] = useState(true);
  const [isProvisioningDefaultDatapack, setIsProvisioningDefaultDatapack] =
    useState(false);
  const [error, setError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<"agents" | "memories" | "knowledge" | null>("agents");
  const [memoriesSortOrder, setMemoriesSortOrder] = useState<"asc" | "desc">("desc");
  const [copiedField, setCopiedField] = useState<string | null>(null);
  const [isExplanationExpanded, setIsExplanationExpanded] = useState(true);
  const [isConnectOpen, setIsConnectOpen] = useState(false);
  const [connectLoading, setConnectLoading] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [provisionStatus, setProvisionStatus] = useState<ProvisionStatus | null>(null);

  const selectedDatapack = useMemo(
    () => datapacks.find((dp) => dp.name === activeDatapack),
    [datapacks, activeDatapack],
  );

  const handleCopy = async (text: string, field: string) => {
    try {
      const copied = await copyToClipboard(text);
      if (!copied) return;
      setCopiedField(field);
      setTimeout(() => setCopiedField(null), 2000);
    } catch {
      setError("Failed to copy to clipboard. Please copy manually.");
    }
  };

  const handleSortMemories = () => {
    setMemoriesSortOrder(prev => prev === "desc" ? "asc" : "desc");
  };

  const sortedMemories = [...MEMORIES].sort((a, b) => {
    const dateA = new Date(a.createdAt).getTime();
    const dateB = new Date(b.createdAt).getTime();
    return memoriesSortOrder === "desc" ? dateB - dateA : dateA - dateB;
  });

  useEffect(() => {
    const loadDatapacks = async () => {
      try {
        setIsLoading(true);
        setError(null);
        const items = await listDatapacks();
        setDatapacks(items);
        if (items.length > 0) {
          setActiveDatapack(items[0].name);
        }
      } catch (err) {
        setError(err instanceof Error ? err.message : "Failed to load datapacks");
      } finally {
        setIsLoading(false);
      }
    };

    loadDatapacks();
  }, []);

  useEffect(() => {
    if (isLoading || datapacks.length > 0) {
      setIsProvisioningDefaultDatapack(false);
      return;
    }

    let mounted = true;
    let attempts = 0;
    const maxAttempts = 30; // ~60s at 2s interval
    setIsProvisioningDefaultDatapack(true);

    const poll = async () => {
      attempts += 1;
      if (!mounted) return;
      try {
        const items = await listDatapacks();
        if (!mounted) return;
        if (items.length > 0) {
          setDatapacks(items);
          setActiveDatapack((current) => current || items[0].name);
          setIsProvisioningDefaultDatapack(false);
          return;
        }
      } catch {
        // Keep polling; initial provisioning can be eventually consistent.
      }
      if (attempts >= maxAttempts && mounted) {
        setIsProvisioningDefaultDatapack(false);
        return;
      }
    };

    const intervalId = globalThis.setInterval(poll, 2000);
    void poll();

    return () => {
      mounted = false;
      globalThis.clearInterval(intervalId);
      setIsProvisioningDefaultDatapack(false);
    };
  }, [isLoading, datapacks.length]);

  useEffect(() => {
    if (!activeDatapack) return;

    const loadDatapackDetails = async () => {
      try {
        setError(null);
        const [agentsData, kbData, statusData] = await Promise.all([
          listAgents(activeDatapack),
          listKnowledgeBases(activeDatapack),
          getDatapackStatus(activeDatapack),
        ]);
        setAgents(agentsData);
        setKnowledgeBases(kbData);
        setDatapackStatus(statusData);
      } catch (err) {
        setError(err instanceof Error ? err.message : "Failed to load datapack details");
      }
    };
    loadDatapackDetails();
  }, [activeDatapack]);

  useEffect(() => {
    const isTechPreviewDatapack =
      activeDatapack === "meko_default_datapack" ||
      activeDatapack.startsWith("meko_default_datapack_");
    if (!isTechPreviewDatapack) {
      return;
    }
    let mounted = true;
    const cache = readLangfuseProjectKeysCache();
    const updatedAt = cache[activeDatapack];
    if (updatedAt && isFresh(updatedAt, LANGFUSE_PROJECT_KEYS_TTL_MS)) {
      return () => {
        mounted = false;
      };
    }

    ensureLangfuseProjectKeys(activeDatapack)
      .then(() => {
        const next = readLangfuseProjectKeysCache();
        next[activeDatapack] = Date.now();
        writeLangfuseProjectKeysCache(next);
      })
      .catch((err) => {
        if (!mounted) return;
        // Keep creation best-effort and avoid surfacing secrets/errors in Datapacks UI.
        console.warn("Langfuse project provisioning failed:", err);
      });
    return () => {
      mounted = false;
    };
  }, [activeDatapack]);

  useEffect(() => {
    if (!isConnectOpen) return;
    let mounted = true;
    const cache = readProvisionStatusCache();
    const cached = activeDatapack ? cache[activeDatapack] : undefined;
    if (cached && mounted) {
      setProvisionStatus(cached);
      setConnectLoading(false);
      setConnectError(null);
      return () => {
        mounted = false;
      };
    }

    const loadProvisionStatus = async () => {
      try {
        setConnectLoading(true);
        setConnectError(null);
        const details = await getProvisionStatus();
        if (!mounted) return;
        setProvisionStatus(details);
        const nextCache = readProvisionStatusCache();
        if (activeDatapack) {
          nextCache[activeDatapack] = details;
        }
        if (details.datapack_name) {
          nextCache[details.datapack_name] = details;
        }
        writeProvisionStatusCache(nextCache);
      } catch (err) {
        if (!mounted) return;
        setProvisionStatus(null);
        if (activeDatapack) {
          const nextCache = readProvisionStatusCache();
          delete nextCache[activeDatapack];
          if (Object.keys(nextCache).length === 0) {
            sessionStorage.removeItem(PROVISION_STATUS_CACHE_KEY);
          } else {
            writeProvisionStatusCache(nextCache);
          }
        }
        setConnectError(
          err instanceof Error ? err.message : "Failed to load connection details",
        );
      } finally {
        if (mounted) setConnectLoading(false);
      }
    };
    loadProvisionStatus();
    return () => {
      mounted = false;
    };
  }, [isConnectOpen, activeDatapack]);

  const handleCreateDatapack = async () => {
    const name = globalThis.prompt("Datapack name (unique)");
    if (!name) return;
    const seedUrl = globalThis.prompt("Seed knowledge base URL", "https://example.com/docs");
    if (!seedUrl) return;

    try {
      setError(null);
      const created = await createDatapack({
        name,
        seed_knowledge_base: seedUrl,
      });
      const refreshed = await listDatapacks();
      setDatapacks(refreshed);
      setActiveDatapack(created.name);
      if (created.mcp_server?.api_key) {
        localStorage.setItem("meko_latest_api_key", created.mcp_server.api_key);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create datapack");
    }
  };

  const showProvisioningScreen =
    !isLoading && datapacks.length === 0 && isProvisioningDefaultDatapack;

  if (showProvisioningScreen) {
    return (
      <div className="flex-1 min-h-screen bg-background text-foreground flex items-center justify-center px-6">
        <div className="w-full max-w-xl rounded-xl border border-blue-500/40 bg-blue-500/10 px-6 py-8 text-center">
          <div className="mx-auto mb-4 h-8 w-8 animate-spin rounded-full border-2 border-blue-300 border-t-transparent" />
          <h2 className="text-lg font-semibold text-blue-100">
            Setting up your default datapack...
          </h2>
          <p className="mt-2 text-sm text-blue-200/80">
            This may take a minute. We will show your datapack as soon as it is ready.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <header className="flex items-center justify-end px-8 py-4 border-b border-border bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/60 sticky top-0 z-10">
        <div className="flex items-center gap-4">
          <Button variant="outline" className="gap-2 text-muted-foreground border-border bg-background hover:bg-muted hover:text-foreground">
            <Terminal className="w-4 h-4" />
            CLI
          </Button>
          <div className="w-8 h-8 rounded-full bg-gradient-to-tr from-blue-500 to-purple-500 flex items-center justify-center text-white text-sm font-medium cursor-pointer">
            A
          </div>
        </div>
      </header>

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8">
        
        {/* Header Section */}
        <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-3">
              <Database className="w-6 h-6 text-blue-400" />
              <Select value={activeDatapack} onValueChange={setActiveDatapack}>
                <SelectTrigger className="w-[300px] border-none shadow-none text-2xl font-semibold tracking-tight h-auto p-0 focus:ring-0 bg-transparent hover:bg-white/5 rounded-md px-2 -ml-2 transition-colors">
                  <SelectValue placeholder="Select Datapack" />
                </SelectTrigger>
                <SelectContent>
                  {datapacks.map(dp => (
                    <SelectItem key={dp.id} value={dp.name} className="font-medium">
                      {dp.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <p className="text-muted-foreground text-sm flex items-center gap-2">
              <span className="w-2 h-2 rounded-full bg-green-500"></span>
              Connected and mapping to logical PostgreSQL DB
            </p>
          </div>
          <div className="flex items-center gap-3">
            <Dialog
              open={isConnectOpen}
              onOpenChange={(open) => {
                setIsConnectOpen(open);
                if (!open) {
                  setConnectError(null);
                  setConnectLoading(false);
                }
              }}
            >
              <DialogTrigger asChild>
                <Button variant="outline" className="gap-2 border-blue-500/30 text-blue-400 hover:bg-blue-500/10">
                  <LinkIcon className="w-4 h-4" />
                  Connect
                </Button>
              </DialogTrigger>
              <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-xl max-h-[calc(100vh-4rem)] bg-[#1C2333] border-border text-foreground p-0 flex flex-col">
                <DialogHeader className="px-6 pt-6 pb-3 shrink-0">
                  <DialogTitle className="text-xl text-white">Connect to Datapack</DialogTitle>
                  <DialogDescription className="text-muted-foreground">
                    Use these credentials to connect your MCP server and database.
                  </DialogDescription>
                </DialogHeader>
                {connectLoading ? (
                  <div className="px-6 pb-6 py-8 text-sm text-muted-foreground text-center overflow-y-auto">
                    Loading connection details...
                  </div>
                ) : connectError ? (
                  <div className="px-6 pb-6 py-6 overflow-y-auto">
                    <p className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">
                      {connectError}
                    </p>
                  </div>
                ) : provisionStatus ? (
                  <div className="space-y-4 px-6 pb-6 py-4 overflow-y-auto min-h-0">
                    <h3 className="text-lg font-semibold text-white">Datapack</h3>
                    <div className="space-y-2">
                      <label className="text-sm font-medium text-muted-foreground">Datapack</label>
                      <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                        <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.datapack_name}</code>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-white" onClick={() => handleCopy(provisionStatus.datapack_name, "dp")}>
                          {copiedField === "dp" ? <Check className="h-4 w-4 text-green-500" /> : <Copy className="h-4 w-4" />}
                        </Button>
                      </div>
                    </div>

                    <h3 className="pt-2 text-lg font-semibold text-white">MCP Server</h3>
                    <div className="space-y-2">
                      <label className="text-sm font-medium text-muted-foreground">URL</label>
                      <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                        <code className="text-xs text-white break-all flex-1 pr-4">{provisionStatus.mcp_server_url}</code>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-white" onClick={() => handleCopy(provisionStatus.mcp_server_url, "mcpurl")}>
                          {copiedField === "mcpurl" ? <Check className="h-4 w-4 text-green-500" /> : <Copy className="h-4 w-4" />}
                        </Button>
                      </div>
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium text-muted-foreground">API Key</label>
                      <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                        <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.mcp_api_key}</code>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-white" onClick={() => handleCopy(provisionStatus.mcp_api_key, "mcpkey")}>
                          {copiedField === "mcpkey" ? <Check className="h-4 w-4 text-green-500" /> : <Copy className="h-4 w-4" />}
                        </Button>
                      </div>
                    </div>

                    <h3 className="pt-2 text-lg font-semibold text-white">Database Access</h3>
                    <div className="space-y-2">
                      <label className="text-sm font-medium text-muted-foreground">Username</label>
                      <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                        <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.db_user_name}</code>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-white" onClick={() => handleCopy(provisionStatus.db_user_name, "dbuser")}>
                          {copiedField === "dbuser" ? <Check className="h-4 w-4 text-green-500" /> : <Copy className="h-4 w-4" />}
                        </Button>
                      </div>
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium text-muted-foreground">Password</label>
                      <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                        <code className="text-sm text-white break-all flex-1 pr-4">{provisionStatus.db_password}</code>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-white" onClick={() => handleCopy(provisionStatus.db_password, "dbpass")}>
                          {copiedField === "dbpass" ? <Check className="h-4 w-4 text-green-500" /> : <Copy className="h-4 w-4" />}
                        </Button>
                      </div>
                    </div>
                    <div className="space-y-2">
                      <label className="text-sm font-medium text-muted-foreground">Conn String</label>
                      <div className="flex items-center p-3 rounded-md bg-[#0E1525] border border-border">
                        <code className="text-xs text-blue-300 break-all flex-1 pr-4">{provisionStatus.db_conn_str}</code>
                        <Button variant="ghost" size="icon" className="h-8 w-8 text-muted-foreground hover:text-white" onClick={() => handleCopy(provisionStatus.db_conn_str, "dbconn")}>
                          {copiedField === "dbconn" ? <Check className="h-4 w-4 text-green-500" /> : <Copy className="h-4 w-4" />}
                        </Button>
                      </div>
                    </div>
                  </div>
                ) : null}
              </DialogContent>
            </Dialog>
            <Button
              size="lg"
              className="gap-2 font-medium bg-white text-black hover:bg-gray-200"
              onClick={handleCreateDatapack}
            >
              <Plus className="w-5 h-5" />
              New Datapack
            </Button>
          </div>
        </div>

        {error && (
          <div className="rounded-md border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-200">
            {error}
          </div>
        )}
        {isProvisioningDefaultDatapack && (
          <div className="rounded-md border border-blue-500/40 bg-blue-500/10 px-4 py-3 text-sm text-blue-200">
            Setting up your default datapack...
          </div>
        )}

        {/* Explain Datapack */}
        <div className="relative overflow-hidden rounded-xl border border-border bg-[#1C2333]">
          <div className="absolute top-0 right-0 w-64 h-64 bg-blue-500/10 rounded-full blur-3xl -translate-y-1/2 translate-x-1/3" />
          <div className="absolute bottom-0 left-0 w-64 h-64 bg-purple-500/10 rounded-full blur-3xl translate-y-1/3 -translate-x-1/3" />
          
          <div className="relative z-10">
            <div
              className="flex items-center justify-between p-4 sm:px-10 cursor-pointer hover:bg-white/5 transition-colors"
              onClick={() => setIsExplanationExpanded(!isExplanationExpanded)}
            >
              <div className="flex items-center gap-3">
                <Sparkles className="w-5 h-5 text-blue-400" />
                <h2 className="text-xl font-semibold text-white">What is a Datapack?</h2>
              </div>
              <Button variant="ghost" size="icon" className="text-muted-foreground hover:text-white">
                {isExplanationExpanded ? <ChevronUp className="w-5 h-5" /> : <ChevronDown className="w-5 h-5" />}
              </Button>
            </div>

            {isExplanationExpanded && (
              <div className="p-4 sm:px-10 sm:pb-10 pt-0 flex flex-col md:flex-row items-center gap-8 animate-in fade-in slide-in-from-top-4 duration-300">
                <div className="flex-1">
                  <p className="text-muted-foreground text-base mb-6 leading-relaxed max-w-2xl">
                    A Datapack is a secure, scalable data store for agents. It handles agent memories, comprehensive knowledge bases, and complex transactional data natively. Each Datapack automatically maps to a dedicated YugabyteDB YSQL logical database under the hood.
                  </p>
                  <div className="flex items-center gap-3">
                    <Button variant="secondary" className="gap-2 text-sm bg-white/10 hover:bg-white/20 text-white border-0">
                      <Book className="w-4 h-4" />
                      Documentation
                    </Button>
                    <Button variant="ghost" className="gap-2 text-sm text-blue-400 hover:text-blue-300 hover:bg-blue-500/10">
                      View Examples
                    </Button>
                  </div>
                </div>
                
                <div className="hidden md:flex w-48 h-32 border border-border bg-[#0E1525] rounded-xl flex-col items-center justify-center gap-4 relative overflow-hidden group">
                  <div className="absolute inset-0 bg-gradient-to-tr from-blue-500/5 to-transparent opacity-0 group-hover:opacity-100 transition-opacity" />
                  <Database className="w-10 h-10 text-blue-400" />
                  <div className="text-center">
                    <div className="text-sm font-medium text-white">PostgreSQL</div>
                    <div className="text-xs text-muted-foreground mt-1">Logical DB Mapping</div>
                  </div>
                </div>
              </div>
            )}
          </div>
        </div>

        {/* Dashboard Overview Cards */}
        <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
          <Card className="relative border-blue-500 bg-blue-500/5 overflow-hidden">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
                <Users className="w-4 h-4" />
                Agents Connected
              </CardTitle>
            </CardHeader>
            <CardContent className="blur-md opacity-35 select-none">
              <div className="text-3xl font-semibold text-white">
                {datapackStatus?.agent_count ?? agents.length}
              </div>
              <p className="text-xs text-muted-foreground mt-1">Active across this datapack</p>
            </CardContent>
            <div className="absolute inset-0 flex items-center justify-center bg-[#0E1525]/45">
              <span className="rounded-full border border-blue-500/30 bg-blue-500/15 px-3 py-1 text-xs font-semibold uppercase tracking-wider text-blue-200">
                Coming Soon
              </span>
            </div>
          </Card>

          <Card className="relative border-border bg-[#1C2333] overflow-hidden">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
                <BrainCircuit className="w-4 h-4" />
                Total Memories
              </CardTitle>
            </CardHeader>
            <CardContent className="blur-sm opacity-60 select-none">
              <div className="text-3xl font-semibold text-white">{sortedMemories.length}</div>
              <p className="text-xs text-muted-foreground mt-1">Stored interactions & context</p>
            </CardContent>
            <div className="absolute inset-0 flex items-center justify-center bg-[#1C2333]/35">
              <span className="rounded-full border border-purple-500/30 bg-purple-500/15 px-3 py-1 text-xs font-semibold uppercase tracking-wider text-purple-200">
                Coming Soon
              </span>
            </div>
          </Card>

          <Card className="relative border-border bg-[#1C2333] overflow-hidden">
            <CardHeader className="pb-2">
              <CardTitle className="text-sm font-medium text-muted-foreground flex items-center gap-2">
                <Library className="w-4 h-4" />
                Knowledge Bases
              </CardTitle>
            </CardHeader>
            <CardContent className="blur-sm opacity-60 select-none">
              <div className="flex items-end gap-3">
                <div className="text-3xl font-semibold text-white">
                  {datapackStatus?.knowledge_base_count ?? knowledgeBases.length}{" "}
                  <span className="text-sm text-muted-foreground font-normal">sources</span>
                </div>
              </div>
              <p className="text-xs text-muted-foreground mt-1">Knowledge sources mapped to this datapack</p>
            </CardContent>
            <div className="absolute inset-0 flex items-center justify-center bg-[#1C2333]/35">
              <span className="rounded-full border border-amber-500/30 bg-amber-500/15 px-3 py-1 text-xs font-semibold uppercase tracking-wider text-amber-200">
                Coming Soon
              </span>
            </div>
          </Card>
        </div>

        {/* Detail Views */}
        <div className="mt-8 border border-border rounded-xl bg-[#0E1525] overflow-hidden">
          {activeTab === "agents" && (
            <div className="relative animate-in fade-in slide-in-from-bottom-4 duration-500">
              <div className="blur-sm opacity-60 select-none">
                <div className="px-6 py-5 border-b border-border bg-[#1C2333]">
                  <h3 className="text-lg font-medium text-white flex items-center gap-2">
                    <Users className="w-5 h-5 text-blue-400" />
                    Agents Dashboard
                  </h3>
                  <p className="text-sm text-muted-foreground mt-1">
                    Manage agents utilizing {selectedDatapack?.name || "selected datapack"}.
                  </p>
                </div>
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent border-border">
                      <TableHead className="w-[150px]">Agent ID</TableHead>
                      <TableHead className="w-[240px]">Agent Name</TableHead>
                      <TableHead>Metadata</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {agents.map((agent) => (
                      <TableRow key={agent.id} className="border-border hover:bg-white/5">
                        <TableCell className="font-medium text-white">{agent.id}</TableCell>
                        <TableCell className="text-muted-foreground">{agent.agent_name}</TableCell>
                        <TableCell className="text-muted-foreground">{agent.agent_metadata || "{}"}</TableCell>
                      </TableRow>
                    ))}
                    {agents.length === 0 && !isLoading && (
                      <TableRow className="border-border">
                        <TableCell colSpan={3} className="text-center text-muted-foreground py-8">
                          No agents available for this datapack.
                        </TableCell>
                      </TableRow>
                    )}
                  </TableBody>
                </Table>
              </div>
              <div className="absolute inset-0 flex items-center justify-center bg-[#0E1525]/35">
                <span className="rounded-full border border-blue-500/30 bg-blue-500/15 px-3 py-1 text-xs font-semibold uppercase tracking-wider text-blue-200">
                  Coming Soon
                </span>
              </div>
            </div>
          )}

          {activeTab === "memories" && (
            <div className="animate-in fade-in slide-in-from-bottom-4 duration-500">
              <div className="px-6 py-5 border-b border-border bg-[#1C2333] flex justify-between items-center">
                <div>
                  <h3 className="text-lg font-medium text-white flex items-center gap-2">
                    <BrainCircuit className="w-5 h-5 text-purple-400" />
                    Agent Memories
                  </h3>
                  <p className="text-sm text-muted-foreground mt-1">
                    Memory APIs are not exposed yet in current meko API server, showing placeholder data.
                  </p>
                </div>
              </div>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent border-border">
                    <TableHead className="w-[150px]">Agent ID</TableHead>
                    <TableHead>Memory Context</TableHead>
                    <TableHead className="text-right">
                      <Button variant="ghost" size="sm" onClick={handleSortMemories} className="h-8 hover:bg-white/10 -mr-3 text-muted-foreground">
                        Created At
                        <ArrowUpDown className="ml-2 h-3.5 w-3.5" />
                      </Button>
                    </TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {sortedMemories.map((memory) => (
                    <TableRow key={memory.id} className="border-border hover:bg-white/5">
                      <TableCell className="font-medium text-white">{memory.agentId}</TableCell>
                      <TableCell className="text-muted-foreground">{memory.context}</TableCell>
                      <TableCell className="text-right text-muted-foreground tabular-nums">
                        {new Date(memory.createdAt).toLocaleString(undefined, {
                          year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit"
                        })}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            </div>
          )}

          {activeTab === "knowledge" && (
            <div className="animate-in fade-in slide-in-from-bottom-4 duration-500">
              <div className="px-6 py-5 border-b border-border bg-[#1C2333]">
                <h3 className="text-lg font-medium text-white flex items-center gap-2">
                  <Library className="w-5 h-5 text-amber-400" />
                  Knowledge Bases
                </h3>
                <p className="text-sm text-muted-foreground mt-1">Vectorized documents and sources mapped to PostgreSQL pgvector.</p>
              </div>
              <Table>
                <TableHeader>
                  <TableRow className="hover:bg-transparent border-border">
                    <TableHead>Source</TableHead>
                    <TableHead>Created</TableHead>
                    <TableHead className="text-right">Documents</TableHead>
                    <TableHead className="text-right">Embeddings</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {knowledgeBases.map((kb) => (
                    <TableRow key={kb.id} className="border-border hover:bg-white/5">
                      <TableCell className="font-medium text-white">{kb.knowledge_base_url}</TableCell>
                      <TableCell>
                        <code className="text-xs bg-muted/50 px-2 py-1 rounded text-blue-300 border border-blue-500/20">
                          {kb.created_at ? new Date(kb.created_at).toLocaleString() : "-"}
                        </code>
                      </TableCell>
                      <TableCell className="text-right text-muted-foreground">-</TableCell>
                      <TableCell className="text-right text-muted-foreground">-</TableCell>
                    </TableRow>
                  ))}
                  {knowledgeBases.length === 0 && !isLoading && (
                    <TableRow className="border-border">
                      <TableCell colSpan={4} className="text-center text-muted-foreground py-8">
                        No knowledge bases available for this datapack.
                      </TableCell>
                    </TableRow>
                  )}
                </TableBody>
              </Table>
            </div>
          )}
        </div>

      </main>
    </div>
  );
}
