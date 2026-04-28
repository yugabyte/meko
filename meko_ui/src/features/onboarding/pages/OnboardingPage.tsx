import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import {
  getApiErrorMessage,
  getCurrentUser,
  ONBOARDING_STATUS_CACHE_KEY,
  ONBOARDING_STATUS_UPDATED_EVENT,
  updateOnboardingStatus,
} from "@/lib/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { DEFAULT_ACCOUNT_DATAPACK_NAME, ROUTES, datapackConfigurePath } from "@/app/routes/paths";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { ArrowRight, Check, Play, Sparkles, Terminal } from "lucide-react";
import { cn } from "@/lib/utils";

export default function OnboardingPage() {
  const navigate = useNavigate();
  const [action, setAction] = useState<"playground" | "datapacks" | "skip" | null>(null);
  const [error, setError] = useState<string | null>(null);
  /** From server: returning users “Choose your tool” → /datapacks; first-time → default datapack edit → configure. */
  const [serverOnboardingDone, setServerOnboardingDone] = useState<boolean | null>(null);

  useEffect(() => {
    let cancelled = false;
    void getCurrentUser()
      .then((u) => {
        if (!cancelled) setServerOnboardingDone(u.has_completed_onboarding);
      })
      .catch(() => {
        if (!cancelled) setServerOnboardingDone(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  const finishAndGo = async (target: "playground" | "datapacks" | "skip") => {
    setError(null);
    setAction(target);
    const previous = localStorage.getItem(ONBOARDING_STATUS_CACHE_KEY);
    localStorage.setItem(ONBOARDING_STATUS_CACHE_KEY, "true");
    dispatchAppEvent(ONBOARDING_STATUS_UPDATED_EVENT, {
      has_completed_onboarding: true,
    });
    const path =
      target === "playground"
        ? ROUTES.playground
        : target === "datapacks"
          ? serverOnboardingDone
            ? ROUTES.datapacks
            : datapackConfigurePath(DEFAULT_ACCOUNT_DATAPACK_NAME)
          : ROUTES.datapacks;
    try {
      await updateOnboardingStatus(true);
      navigate(path, { replace: true });
    } catch (err) {
      if (previous == null) {
        localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
      } else {
        localStorage.setItem(ONBOARDING_STATUS_CACHE_KEY, previous);
      }
      dispatchAppEvent(ONBOARDING_STATUS_UPDATED_EVENT, {
        has_completed_onboarding: previous === "true",
      });
      setError(getApiErrorMessage(err, "Failed to save onboarding status."));
      setAction(null);
    }
  };

  const busy = action != null;
  const chooseToolDisabled = busy || serverOnboardingDone === null;

  return (
    <div className="flex min-h-screen flex-col bg-background text-foreground">
      <AppWorkspaceHeader
        showCliButton={false}
        padding="page"
        className="pl-4 pr-8"
        leading={
          <img src="/meko-logo.svg" alt="meko" className="h-12 w-auto" />
        }
      />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col items-center px-8 pb-16 pt-8">
        <div
          className={cn(
            "mb-8 inline-flex items-center gap-2 rounded-full border border-blue-500/25 bg-blue-500/10 px-4 py-1.5 text-xs font-medium text-blue-300",
          )}
        >
          <Sparkles className="h-3.5 w-3.5 shrink-0 text-blue-400" aria-hidden />
          Your workspace is ready · 1 default datapack created
        </div>

        <h1 className="mb-3 text-center text-3xl font-bold tracking-tight text-foreground md:text-4xl">
          Welcome to Meko.
        </h1>
        <p className="mb-12 max-w-xl text-center text-sm leading-relaxed text-muted-foreground md:text-base">
          Meko is collective memory and shared knowledge for your AI agents. Pick how you want to
          start.
        </p>

        <div className="grid w-full max-w-4xl gap-6 md:grid-cols-2">
          <Card className="flex flex-col border-border bg-card/50 shadow-sm">
            <CardContent className="flex flex-1 flex-col gap-0 p-6">
              <div className="mb-4 flex items-start justify-between gap-3">
                <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-blue-600 text-white shadow-md">
                  <Play className="h-5 w-5 fill-current" aria-hidden />
                </div>
              </div>
              <h2 className="mb-2 text-lg font-semibold tracking-tight text-foreground">
                Try the Playground
              </h2>
              <p className="mb-5 flex-1 text-sm leading-relaxed text-muted-foreground">
                Start with a sample knowledge base (vacation spots). Upload a file, ask a question,
                and watch memories form in real time — all inside your default datapack.
              </p>
              <ul className="mb-6 space-y-2.5 text-sm text-foreground/90">
                {["Add knowledge from a PDF or URL", "Chat and see traces + memories live", "No install, no config"].map(
                  (line) => (
                    <li key={line} className="flex gap-2">
                      <Check className="mt-0.5 h-4 w-4 shrink-0 text-blue-400" aria-hidden />
                      <span>{line}</span>
                    </li>
                  ),
                )}
              </ul>
              <Button
                type="button"
                className="w-full gap-2 bg-blue-600 text-white hover:bg-blue-500"
                disabled={busy}
                onClick={() => void finishAndGo("playground")}
              >
                {action === "playground" ? "Opening…" : "Open Playground"}
                <ArrowRight className="h-4 w-4" />
              </Button>
            </CardContent>
          </Card>

          <Card className="flex flex-col border-border bg-card/50 shadow-sm">
            <CardContent className="flex flex-1 flex-col gap-0 p-6">
              <div className="mb-4 flex items-start justify-between gap-3">
                <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-emerald-600 text-white shadow-md">
                  <Terminal className="h-5 w-5" aria-hidden />
                </div>
              </div>
              <h2 className="mb-2 text-lg font-semibold tracking-tight text-foreground">
                Connect to your coding agent
              </h2>
              <p className="mb-5 flex-1 text-sm leading-relaxed text-muted-foreground">
                Point Claude Code, Codex, or Cursor at an existing project. Meko detects your
                knowledge, conversation history, and skills — and turns them into a live datapack.
              </p>
              <div className="mb-6 flex flex-wrap gap-2">
                {["Claude Code", "OpenAI Codex", "Cursor"].map((label) => (
                  <button
                    key={label}
                    type="button"
                    className="rounded-md border border-border bg-muted/50 px-2.5 py-1 text-[11px] font-medium text-muted-foreground transition hover:bg-muted hover:text-foreground"
                  >
                    {label}
                  </button>
                ))}
              </div>
              <Button
                type="button"
                className="w-full gap-2 bg-emerald-600 text-white hover:bg-emerald-500"
                disabled={chooseToolDisabled}
                onClick={() => void finishAndGo("datapacks")}
              >
                {action === "datapacks" ? "Loading…" : "Choose your tool"}
                <ArrowRight className="h-4 w-4" />
              </Button>
            </CardContent>
          </Card>
        </div>

        <button
          type="button"
          className="mt-10 text-sm text-muted-foreground underline-offset-4 transition hover:text-foreground hover:underline disabled:opacity-50"
          disabled={busy}
          onClick={() => void finishAndGo("skip")}
        >
          {action === "skip" ? "Saving…" : "Skip for now · I'll set this up later"}
          <span className="ml-1" aria-hidden>
            {'>'}
          </span>
        </button>

        {error ? <p className="mt-4 text-center text-sm text-destructive">{error}</p> : null}
      </main>
    </div>
  );
}
