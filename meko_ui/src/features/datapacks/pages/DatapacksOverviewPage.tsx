import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Plus, Database, Sparkles, Book, ChevronDown, ChevronUp, Share2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DATAPACKS_LIST_UPDATED_EVENT,
  getApiErrorMessage,
  listDatapacks,
  type CurrentUser,
  type Datapack,
} from "@/lib/mekoApi";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import { datapackDetailPath, datapackNewConfigurePath, datapackSharePath } from "@/app/routes/paths";
import DatapacksTilesSection from "@/features/datapacks/components/DatapacksTilesSection";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

type OverviewProps = {
  currentUser: CurrentUser | null;
};

export default function DatapacksOverviewPage({ currentUser }: OverviewProps) {
  const navigate = useNavigate();
  const [datapacks, setDatapacks] = useState<Datapack[]>([]);
  const [isLoading, setIsLoading] = useState(true);
  const [isProvisioningDefaultDatapack, setIsProvisioningDefaultDatapack] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [isExplanationExpanded, setIsExplanationExpanded] = useState(true);
  const [sharePickerOpen, setSharePickerOpen] = useState(false);

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

  const handleShareDatapack = () => {
    if (datapacks.length === 0) return;
    if (datapacks.length === 1) {
      navigate(datapackSharePath(datapacks[0].datapack_name));
      return;
    }
    setSharePickerOpen(true);
  };

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
        <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-3">
              <Database className="w-6 h-6 text-blue-400 shrink-0" />
              <h1 className="text-2xl font-semibold tracking-tight">Datapacks</h1>
            </div>
            <p className="text-muted-foreground text-sm max-w-xl">
              All datapacks in your workspace. Open one to manage agents, knowledge, and connection details.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2 shrink-0">
            <Button
              type="button"
              variant="outline"
              size="lg"
              className="gap-2 font-medium border-border bg-background/80"
              disabled={isLoading || datapacks.length === 0}
              onClick={handleShareDatapack}
            >
              <Share2 className="w-5 h-5 shrink-0" aria-hidden />
              Share datapack
            </Button>
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

        <Dialog open={sharePickerOpen} onOpenChange={setSharePickerOpen}>
          <DialogContent className="sm:max-w-md">
            <DialogHeader>
              <DialogTitle>Share a datapack</DialogTitle>
              <DialogDescription>
                Choose which datapack to open sharing settings for.
              </DialogDescription>
            </DialogHeader>
            <ul className="max-h-64 space-y-1 overflow-y-auto pr-1" role="listbox">
              {datapacks.map((dp) => (
                <li key={dp.datapack_id}>
                  <Button
                    type="button"
                    variant="ghost"
                    className="h-auto w-full justify-start px-3 py-2.5 font-normal"
                    onClick={() => {
                      navigate(datapackSharePath(dp.datapack_name));
                      setSharePickerOpen(false);
                    }}
                  >
                    <span className="truncate text-left">{dp.datapack_name}</span>
                  </Button>
                </li>
              ))}
            </ul>
          </DialogContent>
        </Dialog>

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
                      onClick={() => window.open("https://mekodata.netlify.app/", "_blank")}
                    >
                      <Book className="w-4 h-4" />
                      Documentation
                    </Button>
                    <Button
                      variant="ghost"
                      className="gap-2 text-sm text-blue-400 hover:text-blue-300 hover:bg-blue-500/10"
                      onClick={() => window.open("https://mekodata.netlify.app/examples/", "_blank")}
                    >
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
            ) : null}
          </div>
        </div>

        <DatapacksTilesSection
          datapacks={datapacks}
          isLoading={isLoading}
          currentUser={currentUser}
          onOpenDatapack={(dp) => navigate(datapackDetailPath(dp.datapack_name))}
        />
      </main>
    </div>
  );
}
