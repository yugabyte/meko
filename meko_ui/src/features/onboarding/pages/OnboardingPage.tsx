import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ArrowLeftRight,
  ArrowRight,
  BookOpen,
  ChevronRight,
  Database,
  ExternalLink,
  Package,
  Play,
  Share2,
  Sparkles,
  Terminal,
  Users,
  Workflow,
} from "lucide-react";
import {
  getApiErrorMessage,
  getCurrentUser,
  ONBOARDING_STATUS_CACHE_KEY,
  ONBOARDING_STATUS_UPDATED_EVENT,
  updateOnboardingStatus,
} from "@/lib/api/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { ROUTES } from "@/app/routes/paths";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { cn } from "@/lib/utils";
import ConfirmNameModal from "@/features/onboarding/components/ConfirmNameModal";

// "How it works" storyboard. Hoisted out of the component so the array
// isn't re-allocated on every render. Each entry maps to one numbered
// card in the onboarding grid. `chips` describes the optional small
// labels below the body — `joinedBy` is the separator text rendered
// BEFORE each chip from index 1 onward (we use "or" for alternatives in
// step 1 and "+" for combinations in steps 3/4, mirroring how the
// product surfaces actually compose).
const STEPS: Array<{
  n: number;
  title: string;
  body: string;
  Icon: typeof Workflow;
  iconBg: string;
  iconRing: string;
  iconColor: string;
  /**
   * "stack" → vertical, one chip per row, no separator (use for
   *    alternatives like "WorkbenchLM" / "Connect agent" where the user
   *    picks exactly one path).
   * "inline" (default) → horizontal row with a `joinedBy` separator
   *    rendered between chips (use for additive combos like
   *    "Knowledge + Memory" or "Promote + Share").
   */
  chipsLayout?: "stack" | "inline";
  chips?: Array<{
    label: string;
    Icon: typeof Play;
    tone: string;
    joinedBy: string;
  }>;
}> = [
  {
    n: 1,
    title: "Pick your journey",
    body: "Explore in the WorkbenchLM with sample data, or connect Meko to your existing agentic app.",
    Icon: Workflow,
    iconBg: "bg-cyan-500/10",
    iconRing: "ring-cyan-500/30",
    iconColor: "text-cyan-700 dark:text-cyan-300",
    chipsLayout: "stack",
    chips: [
      {
        label: "WorkbenchLM",
        Icon: Play,
        tone: "border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-300",
        joinedBy: "",
      },
      {
        label: "Connect agent",
        Icon: Terminal,
        tone: "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
        joinedBy: "",
      },
    ],
  },
  {
    n: 2,
    title: "Select a datapack",
    body: "A workspace for your project's memory, knowledge, and decision traces. One per project, or one per team.",
    Icon: Database,
    iconBg: "bg-blue-500/10",
    iconRing: "ring-blue-500/30",
    iconColor: "text-blue-700 dark:text-blue-300",
  },
  {
    n: 3,
    title: "Store context",
    body: "Upload knowledge files. Let conversations build memories automatically as your agent works.",
    Icon: Package,
    iconBg: "bg-emerald-500/10",
    iconRing: "ring-emerald-500/30",
    iconColor: "text-emerald-700 dark:text-emerald-300",
    chips: [
      {
        label: "Knowledge",
        Icon: BookOpen,
        tone: "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300",
        joinedBy: "+",
      },
      {
        label: "Memory",
        Icon: Sparkles,
        tone: "border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-300",
        joinedBy: "+",
      },
    ],
  },
  {
    n: 4,
    title: "Promote and share",
    body: "Lift recurring patterns into collective learnings. Share the datapack with teammates.",
    Icon: Users,
    iconBg: "bg-purple-500/10",
    iconRing: "ring-purple-500/30",
    iconColor: "text-purple-700 dark:text-purple-300",
    chips: [
      {
        label: "Promote",
        Icon: ArrowLeftRight,
        tone: "border-purple-500/30 bg-purple-500/10 text-purple-700 dark:text-purple-300",
        joinedBy: "+",
      },
      {
        label: "Share",
        Icon: Share2,
        tone: "border-purple-500/30 bg-purple-500/10 text-purple-700 dark:text-purple-300",
        joinedBy: "+",
      },
    ],
  },
];

export default function OnboardingPage() {
  const navigate = useNavigate();
  const [action, setAction] = useState<"workbenchLm" | "datapacks" | "skip" | null>(null);
  const [error, setError] = useState<string | null>(null);
  /** From server: drives first-time vs returning copy; “Choose your tool” path uses configure for first-time only. */
  const [serverOnboardingDone, setServerOnboardingDone] = useState<boolean | null>(null);
  /**
   * Pre-fill values + visibility for the name-confirmation modal. Driven
   * purely by server state (`/auth/me`, no cookie): the modal shows only when
   * a name part is missing. Email/password signup requires both names, so
   * those users never see it — even if they later sign in with Google before
   * finishing onboarding (the PreSignUp link resolves the Google login to
   * their existing account, which already has the names). In practice the
   * modal only appears for a social signup whose IdP returned an incomplete
   * name. When shown, it BLOCKS the page until both parts are filled (no
   * skip); a user who abandons it is re-prompted next visit because the DB
   * still has the gap.
   */
  const [confirmName, setConfirmName] = useState<{ first: string; last: string } | null>(null);

  useEffect(() => {
    let cancelled = false;
    void getCurrentUser()
      .then((u) => {
        if (cancelled) return;
        setServerOnboardingDone(u.has_completed_onboarding);
        const first = u.first_name ?? "";
        const last = u.last_name ?? "";
        if (first.trim() === "" || last.trim() === "") {
          setConfirmName({ first, last });
        }
      })
      .catch(() => {
        if (!cancelled) setServerOnboardingDone(false);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // Only called after a successful save (the modal can't be dismissed any
  // other way), so by the time we clear it the DB has both name parts.
  const dismissConfirmName = () => {
    setConfirmName(null);
  };

  const finishAndGo = async (target: "workbenchLm" | "datapacks" | "skip") => {
    setError(null);
    setAction(target);
    const path =
      target === "workbenchLm"
        ? ROUTES.workbenchLm
        : target === "datapacks"
          ? // "Connect Meko to your agent" CTA → dedicated /connect page.
            // The Connect page (sidebar item) is the canonical surface
            // for MCP install + API-key generation; sending the user
            // there is more discoverable than auto-opening a dialog on
            // /datapacks.
            ROUTES.connect
          : ROUTES.datapacks;

    // Fast path: onboarding already completed on the server. Skip the
    // PATCH entirely — re-flipping a flag that's already true is a wasted
    // round-trip and (more importantly) leaves the user stuck on
    // "Opening…" if the call hangs for any reason.
    if (serverOnboardingDone) {
      navigate(path, { replace: true });
      return;
    }

    const previous = localStorage.getItem(ONBOARDING_STATUS_CACHE_KEY);
    localStorage.setItem(ONBOARDING_STATUS_CACHE_KEY, "true");
    dispatchAppEvent(ONBOARDING_STATUS_UPDATED_EVENT, {
      has_completed_onboarding: true,
    });
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
      {confirmName ? (
        <ConfirmNameModal
          initialFirstName={confirmName.first}
          initialLastName={confirmName.last}
          onDone={dismissConfirmName}
        />
      ) : null}
      <AppWorkspaceHeader padding="page" className="pl-4 pr-8" />

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col items-center px-8 pb-16 pt-8">
        {serverOnboardingDone === null ? (
          <p className="py-24 text-center text-sm text-muted-foreground">Loading…</p>
        ) : (
          <>
            <div
              className={cn(
                "mb-8 inline-flex items-center gap-2 rounded-full border border-blue-500/25 bg-blue-500/10 px-4 py-1.5 text-xs font-medium text-blue-700 dark:text-blue-300",
              )}
            >
              <Sparkles className="h-3.5 w-3.5 shrink-0 text-blue-400" aria-hidden />
              {serverOnboardingDone
                ? "Welcome back — pick where to go next"
                : "Your workspace is ready · 1 default datapack created"}
            </div>

            <h1 className="mb-3 text-center text-3xl font-bold tracking-tight text-foreground md:text-4xl">
              Shared Context across all your agents.
            </h1>
            {/* Tagline subhead — single-sentence framing that sits between
                the punchy headline and the longer body paragraph. Same
                muted colour as the body so it reads as one block of
                supporting copy, but a step up in size + weight so the
                visual hierarchy stays h1 → tagline → body. */}
            <p className="mb-4 text-center text-lg font-medium text-muted-foreground md:text-xl">
              The data layer for agents that learn together.
            </p>
            <p className="mb-12 max-w-2xl text-center text-sm leading-relaxed text-muted-foreground md:text-base">
              Teach an agent something in one tool, open another tomorrow, and you explain it all
              over again — every AI tool keeps its own context and none of them talk. Meko is the
              shared memory and knowledge layer your agents read and write across any MCP tool.
              Private by default, shared by choice.
            </p>

            {/* "How it works" — a 4-step storyboard of the Meko mental
                model. Lives between the intro paragraph and the two
                primary CTAs so a brand-new user can scan the whole loop
                (pick lane → make datapack → context flows in → promote &
                share) before being asked to act. Each step's title +
                supporting chips map 1:1 to product surfaces, so the user
                also picks up the vocabulary (datapack, knowledge,
                memory, promote, share) just by reading. */}
            <section className="w-full max-w-6xl">
              <div className="mb-5 flex items-center gap-2.5">
                <span className="h-3 w-0.5 rounded-full bg-blue-500/70" aria-hidden />
                <h2 className="text-[11px] font-semibold uppercase tracking-[0.2em] text-muted-foreground">
                  How it works
                </h2>
              </div>

              <div className="mb-10 grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
                {STEPS.map((step) => (
                  <div
                    key={step.n}
                    className="relative flex flex-col rounded-xl border border-border bg-card/40 p-5"
                  >
                    <div className="mb-4 flex items-start justify-between gap-3">
                      <div
                        className={cn(
                          "flex h-11 w-11 items-center justify-center rounded-lg ring-1",
                          step.iconBg,
                          step.iconRing,
                        )}
                      >
                        <step.Icon className={cn("h-5 w-5", step.iconColor)} aria-hidden />
                      </div>
                      {/* Step badge — bumped from a bare numeric circle to
                          a labelled "STEP N" pill (brighter foreground +
                          tinted background) so the four cards visibly
                          read as a sequence and not as four unrelated
                          feature tiles. Earlier review feedback called
                          out that the muted "1 / 2 / 3 / 4" circles
                          didn't communicate ordering strongly enough. */}
                      <span
                        className="inline-flex shrink-0 items-center gap-1 rounded-full border border-foreground/20 bg-foreground/[0.06] px-2 py-0.5 text-[10px] font-semibold uppercase tracking-[0.14em] text-foreground/85"
                        aria-label={`Step ${step.n}`}
                      >
                        Step {step.n}
                      </span>
                    </div>
                    <h3 className="mb-1.5 text-base font-semibold tracking-tight text-foreground">
                      {step.title}
                    </h3>
                    <p className="mb-4 flex-1 text-sm leading-relaxed text-muted-foreground">
                      {step.body}
                    </p>
                    {step.chips ? (
                      // Two layouts:
                      //   - "stack"  → vertical, one chip per row, no
                      //                separator. Used for alternatives
                      //                (the user picks one path).
                      //   - "inline" → horizontal row with a `joinedBy`
                      //                separator between chips. Used for
                      //                additive combos ("A + B").
                      // `whitespace-nowrap` on the chip keeps the label
                      // from breaking mid-word if the card is narrow.
                      <div
                        className={cn(
                          step.chipsLayout === "stack"
                            ? "flex flex-col items-start gap-1.5"
                            : "flex items-center gap-1.5",
                        )}
                      >
                        {step.chips.map((chip, idx) => (
                          <span key={chip.label} className="inline-flex items-center gap-1.5">
                            {idx > 0 && step.chipsLayout !== "stack" ? (
                              <span className="text-xs text-muted-foreground/70" aria-hidden>
                                {chip.joinedBy}
                              </span>
                            ) : null}
                            <span
                              className={cn(
                                "inline-flex items-center gap-1.5 whitespace-nowrap rounded-md border px-2 py-1 text-[11px] font-medium",
                                chip.tone,
                              )}
                            >
                              <chip.Icon className="h-3 w-3" aria-hidden />
                              {chip.label}
                            </span>
                          </span>
                        ))}
                      </div>
                    ) : null}
                  </div>
                ))}
              </div>

              <p className="mb-6 text-center text-sm text-muted-foreground">
                <span className="font-medium text-foreground">~5 minutes to set up.</span>
              </p>

              {/* Two primary lanes — playground (low-commitment) on the
                  left, "connect your own agent" (the real product) on the
                  right with the heavier blue treatment. */}
              <div className="mx-auto mb-6 grid w-full max-w-3xl grid-cols-1 gap-4 md:grid-cols-2">
                <button
                  type="button"
                  onClick={() => void finishAndGo("workbenchLm")}
                  disabled={busy}
                  className="group flex items-center gap-4 rounded-xl border border-border bg-card/40 p-5 text-left transition-colors hover:border-border/60 hover:bg-card/70 disabled:cursor-not-allowed disabled:opacity-60"
                >
                  <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-lg border border-border bg-background">
                    <Play className="h-5 w-5 text-foreground" aria-hidden />
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="block text-base font-semibold text-foreground">
                      {action === "workbenchLm" ? "Opening…" : "Try the WorkbenchLM"}
                    </span>
                    <span className="mt-0.5 block text-xs text-muted-foreground">
                      Sample dataset · no setup
                    </span>
                  </span>
                </button>

                {/* Primary CTA. Previously this was solid `bg-blue-600` +
                    white text — fine in dark mode but visually shouting
                    in light mode. Switched to a tinted-fill treatment
                    that uses opacity over a single base hue, so the same
                    classes adapt to both modes: light → soft blue panel
                    with deep-blue text; dark → translucent blue panel
                    with light-blue text. Still clearly the "this is the
                    primary action" lane, just not yelling. */}
                <button
                  type="button"
                  onClick={() => void finishAndGo("datapacks")}
                  disabled={chooseToolDisabled}
                  className="group flex items-center gap-4 rounded-xl border border-blue-500/30 bg-blue-500/10 p-5 text-left transition-colors hover:border-blue-500/50 hover:bg-blue-500/20 dark:border-blue-500/40 dark:bg-blue-500/15 dark:hover:bg-blue-500/25 disabled:cursor-not-allowed disabled:opacity-60"
                >
                  <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-lg border border-blue-500/40 bg-blue-500/15 dark:border-blue-400/40 dark:bg-blue-500/25">
                    <Terminal className="h-5 w-5 text-blue-700 dark:text-blue-200" aria-hidden />
                  </span>
                  <span className="min-w-0 flex-1">
                    <span className="flex items-center gap-2 text-base font-semibold text-blue-900 dark:text-blue-100">
                      {action === "datapacks" ? "Loading…" : "Connect Meko to your agent"}
                      <ArrowRight
                        className="h-4 w-4 shrink-0 transition-transform group-hover:translate-x-0.5"
                        aria-hidden
                      />
                    </span>
                    <span className="mt-0.5 block text-xs text-blue-900/70 dark:text-blue-200/80">
                      Claude Code · Cursor · custom
                    </span>
                  </span>
                </button>
              </div>

              <div className="flex flex-wrap items-center justify-center gap-3 text-sm">
                <a
                  href="https://docs.mekodata.ai/"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="inline-flex items-center gap-1.5 text-muted-foreground transition hover:text-foreground"
                >
                  <ExternalLink className="h-3.5 w-3.5" aria-hidden />
                  View documentation
                </a>
                {/* Skip link is ungated for now — shown to both first-time
                    and returning users. `finishAndGo("skip")` no-ops the
                    PATCH when onboarding is already complete, so clicking
                    it just navigates the user to /datapacks; the label
                    "I'll set up later" reads a bit odd for a returning
                    user but is acceptable for now. */}
                <span className="text-muted-foreground/40" aria-hidden>
                  ·
                </span>
                <button
                  type="button"
                  onClick={() => void finishAndGo("skip")}
                  disabled={busy}
                  className="inline-flex items-center gap-1 text-muted-foreground transition hover:text-foreground disabled:opacity-50"
                >
                  <ChevronRight className="h-3.5 w-3.5" aria-hidden />
                  {action === "skip" ? "Saving…" : "Skip · I'll set up later"}
                </button>
              </div>
            </section>

            {error ? <p className="mt-4 text-center text-sm text-destructive">{error}</p> : null}
          </>
        )}
      </main>
    </div>
  );
}
