// "Get started" chip row, shown on a datapack with no content, no searches and
// no quality data yet — the three things worth doing first, each a link.

import { ArrowRight } from "lucide-react";
import type { NavigateFunction } from "react-router-dom";

import { cn } from "@/lib/utils";
import {
  datapackDetailUrlOpenConnect,
  datapackKnowledgePath,
  ROUTES,
} from "@/app/routes/paths";

/** Accents are semantic tokens, not palette classes: loading documents is a
 *  knowledge action, Labs teaches the learning loop, and connecting an agent
 *  is the product's primary call to action. */
type ChipAccent = "knowledge" | "primary" | "learning";

const ACCENT_CLS: Record<ChipAccent, string> = {
  knowledge: "border-context-knowledge/40 text-context-knowledge hover:border-context-knowledge/70",
  primary: "border-primary/40 text-primary hover:border-primary/70",
  learning: "border-context-learning/40 text-context-learning hover:border-context-learning/70",
};

function StepChip({
  label,
  accent,
  path,
  navigate,
}: {
  label: string;
  accent: ChipAccent;
  path: string | null;
  navigate: NavigateFunction;
}) {
  return (
    <button
      type="button"
      disabled={!path}
      onClick={() => path && navigate(path)}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium transition-colors",
        path ? "cursor-pointer hover:bg-muted/60" : "cursor-default opacity-40",
        ACCENT_CLS[accent],
      )}
    >
      {label}
      {path ? <ArrowRight className="h-3 w-3" aria-hidden /> : null}
    </button>
  );
}

export function DatapackOnboardingStepper({
  datapackId,
  navigate,
}: {
  datapackId: string | null;
  navigate: NavigateFunction;
}) {
  const connectPath = datapackId ? datapackDetailUrlOpenConnect(datapackId) : ROUTES.connect;

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-border bg-card px-4 py-3">
      <span className="shrink-0 text-xs text-muted-foreground">Get started:</span>
      <StepChip
        label="Add knowledge documents"
        accent="knowledge"
        path={datapackId ? datapackKnowledgePath(datapackId) : null}
        navigate={navigate}
      />
      <span className="shrink-0 text-[10px] text-muted-foreground/50">·</span>
      <StepChip
        label="Connect with your agents"
        accent="primary"
        path={connectPath}
        navigate={navigate}
      />
      <span className="shrink-0 text-[10px] font-medium text-muted-foreground/60">or</span>
      <StepChip label="Try Meko Labs" accent="learning" path={ROUTES.labs} navigate={navigate} />
    </div>
  );
}
