import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Plus, Database, Sparkles, Book, ChevronDown, ChevronUp, Layers } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DATAPACKS_LIST_UPDATED_EVENT,
  getApiErrorMessage,
  listDatapacks,
  type CurrentUser,
  type Datapack,
} from "@/lib/mekoApi";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import {
  datapackDetailPath,
  datapackDetailUrlOpenConnect,
  datapackNewConfigurePath,
  datapackSharePath,
} from "@/app/routes/paths";
import DatapacksTilesSection from "@/features/datapacks/components/DatapacksTilesSection";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";

type OverviewProps = {
  currentUser: CurrentUser | null;
};

export default function DatapacksOverviewPage({ currentUser }: OverviewProps) {
  const navigate = useNavigate();
  const [datapacks, setDatapacks] = useState<Datapack[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [isProvisioningDefaultDatapack, setIsProvisioningDefaultDatapack] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [isExplanationExpanded, setIsExplanationExpanded] = useState(false);

  useEffect(() => {
    const loadDatapacks = async () => {
      try {
        setIsLoading(true);
        setError(null);
        const items = await listDatapacks();
        setDatapacks(items);
      } catch (err) {
        setError(getApiErrorMessage(err, "Failed to load datapacks."));
      } finally {
        setIsLoading(false);
      }
    };

    void loadDatapacks();
  }, []);

  useEffect(() => {
    const refreshDatapacks = () => {
      void listDatapacks()
        .then((items) => {
          setDatapacks(items);
          setError(null);
        })
        .catch((err) => {
          setError(getApiErrorMessage(err, "Failed to load datapacks."));
        });
    };
    addAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, refreshDatapacks);
    return () => removeAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, refreshDatapacks);
  }, []);

  useEffect(() => {
    if (isLoading || datapacks.length > 0) {
      setIsProvisioningDefaultDatapack(false);
      return;
    }

    let mounted = true;
    let attempts = 0;
    const maxAttempts = 30;
    setIsProvisioningDefaultDatapack(true);

    const poll = async () => {
      attempts += 1;
      if (!mounted) return;
      try {
        const items = await listDatapacks();
        if (!mounted) return;
        if (items.length > 0) {
          setDatapacks(items);
          setIsProvisioningDefaultDatapack(false);
          return;
        }
      } catch {
        // keep polling
      }
      if (attempts >= maxAttempts && mounted) {
        setIsProvisioningDefaultDatapack(false);
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

  /** Shared rows: not owner (`grant` ≠ owner) and invite grant viewer | contributor | maintainer. */
  const sharedWithYouCount = useMemo(
    () =>
      datapacks.filter((dp) => {
        if (isDatapackListGrantOwner(dp)) return false;
        return true;
      }).length,
    [datapacks],
  );

  const showProvisioningScreen =
    !isLoading && datapacks.length === 0 && isProvisioningDefaultDatapack;

  if (showProvisioningScreen) {
    return (
      <div className="flex-1 min-h-screen bg-background text-foreground flex items-center justify-center px-6">
        <div className="w-full max-w-xl rounded-xl border border-blue-500/40 bg-blue-500/10 px-6 py-8 text-center">
          <div className="mx-auto mb-4 h-8 w-8 animate-spin rounded-full border-2 border-blue-300 border-t-transparent" />
          <h2 className="text-lg font-semibold text-blue-100">Setting up your default datapack...</h2>
          <p className="mt-2 text-sm text-blue-200/80">
            This may take a minute. We will show your datapack as soon as it is ready.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8 pb-16">
        <div className="space-y-4">
          <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-3">
                <div
                  className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/15 ring-1 ring-blue-500/20"
                  aria-hidden
                >
                  <Database className="h-5 w-5 text-blue-400" />
                </div>
                <h1 className="text-2xl font-semibold tracking-tight text-foreground">Datapacks</h1>
              </div>
              <p className="text-muted-foreground text-sm max-w-xl leading-relaxed">
                All datapacks in your workspace. Open one to manage agents, knowledge, memories, and
                connection details.
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2 shrink-0">
              <Button
                size="lg"
                className="gap-2 font-medium bg-white text-black hover:bg-gray-200 disabled:opacity-70"
                onClick={() => navigate(datapackNewConfigurePath())}
              >
                <Plus className="w-5 h-5 shrink-0" aria-hidden />
                Add Datapack
              </Button>
            </div>
          </div>

          {/* <div className="flex flex-wrap items-center justify-between gap-4 rounded-xl border border-border bg-card/50 px-4 py-3 text-sm">
            <div className="flex min-w-0 items-center gap-2 border-border sm:border-r sm:pr-4">
              <Layers className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
              <span className="font-medium text-foreground">Workspace total</span>
            </div>
            <div className="text-muted-foreground">
              <span className="tabular-nums text-foreground/90">{sharedWithYouCount}</span>
              {" shared with me"}
            </div>
          </div> */}
        </div>

        {error ? (
          <div className="rounded-md border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-200">
            {error}
          </div>
        ) : null}
        {isProvisioningDefaultDatapack ? (
          <div className="rounded-md border border-blue-500/40 bg-blue-500/10 px-4 py-3 text-sm text-blue-200">
            Setting up your default datapack...
          </div>
        ) : null}

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

            {isExplanationExpanded ? (
              <div className="p-4 sm:px-10 sm:pb-10 pt-0 flex flex-col md:flex-row items-center gap-8 animate-in fade-in slide-in-from-top-4 duration-300">
                <div className="flex-1">
                  <p className="text-muted-foreground text-base mb-6 leading-relaxed max-w-2xl">
                    A Datapack is a secure, scalable data store for agents. It handles agent memories,
                    comprehensive knowledge bases, and complex transactional data natively. Each Datapack
                    automatically maps to a dedicated YugabyteDB YSQL logical database under the hood.
                  </p>
                  <div className="flex items-center gap-3">
                    <Button
                      variant="secondary"
                      className="gap-2 text-sm bg-white/10 hover:bg-white/20 text-white border-0"
                      onClick={() => window.open("https://docs.mekodata.ai/", "_blank")}
                    >
                      <Book className="w-4 h-4" />
                      Documentation
                    </Button>
                    <Button
                      variant="ghost"
                      className="gap-2 text-sm text-blue-400 hover:text-blue-300 hover:bg-blue-500/10"
                      onClick={() => window.open("https://docs.mekodata.ai/examples/", "_blank")}
                    >
                      View Examples
                    </Button>
                  </div>
                </div>

                <div className="hidden md:flex w-48 h-38 border border-border bg-[#0E1525] rounded-xl flex-col items-center justify-center gap-4 relative overflow-hidden group">
                  <div className="absolute inset-0 bg-gradient-to-tr from-blue-500/5 to-transparent opacity-0 group-hover:opacity-100 transition-opacity" />
                  <Database className="w-10 h-10 text-blue-400" />
                  <div className="text-center">
                    <div className="text-sm font-medium text-white">PostgreSQL</div>
                    <div className="text-xs text-muted-foreground mt-1">Logical DB Mapping</div>
                    <div className="mt-2 flex justify-center">
                      <span className="rounded-md border border-border/70 bg-[#1C2333]/90 px-2 py-0.5 font-mono text-[10px] leading-tight text-muted-foreground/90">
                        Via YugabyteDB mapping
                      </span>
                    </div>
                  </div>
                </div>
              </div>
            ) : null}
          </div>
        </div>

        <DatapacksTilesSection
          datapacks={datapacks}
          isLoading={isLoading}
          currentUser={currentUser}
          onOpenDatapack={(dp) =>
            // datapackDetailPath now accepts the id and appends
            // ?datapack_id= via the shared `appendDatapackId` helper;
            // manual concat is no longer needed.
            navigate(datapackDetailPath(dp.datapack_name, dp.datapack_id))
          }
          onConnectDatapack={(dp) =>
            navigate(datapackDetailUrlOpenConnect(dp.datapack_name, dp.datapack_id))
          }
          onShareDatapack={(dp) => navigate(datapackSharePath(dp.datapack_name))}
          onNewDatapack={() => navigate(datapackNewConfigurePath())}
        />
      </main>
    </div>
  );
}
