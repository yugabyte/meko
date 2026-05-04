import { useEffect, useMemo, useState } from "react";
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
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
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
import { DatapackGrantBadge } from "@/features/datapacks/components/datapack-share/DatapackGrantBadge";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { sortDatapacksByCreatedAtDesc } from "@/lib/datapackDateFormat";
import { isDatapackGrantEligibleToShare, isDatapackShareGrant } from "@/lib/datapackShareGrants";

const SHARE_ACCESS_TOOLTIP =
  "You need Owner or Maintainer access to open sharing settings for this datapack.";

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

  const sortedDatapacksForShareDialog = useMemo(
    () => sortDatapacksByCreatedAtDesc(datapacks),
    [datapacks],
  );

  const handleShareDatapack = () => {
    if (datapacks.length === 0) return;
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
          <DialogContent
            className="w-[calc(100vw-1.5rem)] max-w-lg gap-0 overflow-hidden rounded-2xl border-border bg-card p-0 shadow-xl"
            onOpenAutoFocus={(e) => {
              const root = e.currentTarget as HTMLElement | null;
              if (!root) return;
              const firstShareable = root.querySelector<HTMLButtonElement>('ul button[type="button"]');
              if (firstShareable) {
                e.preventDefault();
                firstShareable.focus();
                return;
              }
              const heading = root.querySelector<HTMLElement>("[data-share-dialog-heading]");
              if (heading) {
                e.preventDefault();
                heading.focus();
              }
            }}
          >
            <div className="border-b border-border/80 bg-muted/40 px-6 pb-4 pt-6">
              <div className="flex items-start gap-3">
                <div className="flex h-10 w-10 shrink-0 items-center justify-center rounded-xl bg-primary/15 text-primary">
                  <Share2 className="h-5 w-5" aria-hidden />
                </div>
                <DialogHeader className="flex-1 space-y-1.5 text-left">
                  <DialogTitle
                    tabIndex={-1}
                    data-share-dialog-heading
                    className="text-lg font-semibold tracking-tight outline-none"
                  >
                    Share a datapack
                  </DialogTitle>
                  <DialogDescription className="text-sm leading-relaxed text-muted-foreground">
                    Your access level is shown for each datapack. Only Owner or Maintainer can open sharing
                    settings.
                  </DialogDescription>
                </DialogHeader>
              </div>
            </div>
            <div
              className="max-h-[min(28rem,70vh)] overflow-y-auto overscroll-contain px-3 py-3 sm:px-4"
              role="listbox"
              aria-label="Datapacks and your access"
            >
              <ul className="space-y-1.5">
                {sortedDatapacksForShareDialog.map((dp) => {
                  const grant =
                    dp.grant != null && isDatapackShareGrant(dp.grant) ? dp.grant : null;
                  const canShare = isDatapackGrantEligibleToShare(dp);

                  const rowInner = (
                    <>
                      <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
                        {dp.datapack_name}
                      </span>
                      {grant ? <DatapackGrantBadge grant={grant} /> : null}
                    </>
                  );

                  return (
                    <li key={dp.datapack_id}>
                      {canShare ? (
                        <button
                          type="button"
                          className="flex w-full items-center gap-3 rounded-xl border border-transparent bg-muted/20 px-4 py-3 text-left transition-colors hover:border-border hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background"
                          onClick={() => {
                            navigate(datapackSharePath(dp.datapack_name));
                            setSharePickerOpen(false);
                          }}
                        >
                          {rowInner}
                        </button>
                      ) : (
                        <Tooltip delayDuration={400}>
                          <TooltipTrigger asChild>
                            <div
                              role="option"
                              aria-disabled
                              tabIndex={0}
                              className="flex w-full cursor-not-allowed items-center gap-3 rounded-xl border border-transparent bg-muted/10 px-4 py-3 text-left opacity-60 outline-none ring-offset-background focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
                            >
                              {rowInner}
                            </div>
                          </TooltipTrigger>
                          <TooltipContent
                            side="top"
                            align="center"
                            className="max-w-[240px] text-center leading-snug"
                          >
                            {SHARE_ACCESS_TOOLTIP}
                          </TooltipContent>
                        </Tooltip>
                      )}
                    </li>
                  );
                })}
              </ul>
            </div>
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
