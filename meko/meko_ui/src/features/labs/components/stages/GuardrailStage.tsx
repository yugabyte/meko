// Chapter 2's autonomy lesson: where the agent stops and asks a human.
//
// The three levels all behave identically in this lab, because AgentK
// reviews here rather than posting to a pull request, so there is nothing
// for a guardrail to hold back. That is stated on the page rather than
// implied: a control that looks wired up and is not is worse than an honest
// one that says so.

import { useState } from "react";
import { AlertTriangle, ArrowUp, Check, ChevronDown, ChevronUp } from "lucide-react";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

import { LabNote } from "./LabNote";
import { LabStageHeader } from "./LabStageHeader";

type Autonomy = "auto" | "comment" | "draft";

/** The middle rung is the default: the agent does the work and you still read
 *  it, which is where a team lands while the datapack is young. */
const DEFAULT_AUTONOMY: Autonomy = "comment";

/** What the same agent did on a repo with no standards doc and no memories.
 *  The lesson's counterweight: the gate is not bureaucracy, it is the thing
 *  that makes letting go survivable. */
const EARLY_HANDOVER_STATS = [
  { value: "23", label: "comments posted", bad: true },
  { value: "14", label: "wrong or irrelevant", bad: true },
  { value: "2.4h", label: "developer time arguing back", bad: true },
  { value: "1", label: "real bug, missed", bad: false },
];

export function GuardrailStage({ agentDisplayName }: { agentDisplayName: string }) {
  const [autonomy, setAutonomy] = useState<Autonomy>(DEFAULT_AUTONOMY);
  const [riskOpen, setRiskOpen] = useState(false);

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 4"
        title="Hand over the wheel"
        lede={`Human-in-the-loop usually means a person drives and the agent assists at each step. Invert it: ${agentDisplayName} drives the stage end to end and comes back to you at the two moments that actually need a human.`}
        tags={["Velocity"]}
      />

      <div className="flex flex-col gap-3">
        <ChoiceCard
          title="Agent drives, you clear blockers"
          tag="fastest"
          locked
          body={`It reviews, comments and files findings unattended. It stops only for a merge-blocking call and for promoting a learning to the team.`}
          selected={autonomy === "auto"}
          onSelect={() => setAutonomy("auto")}
        />
        <ChoiceCard
          title="Agent comments, you read everything"
          tag="recommended"
          body="Safe middle ground while you are still building the datapack. Costs you a read on every PR."
          selected={autonomy === "comment"}
          onSelect={() => setAutonomy("comment")}
        />
        <ChoiceCard
          title="Draft only"
          body="Nothing reaches the pull request until you approve each comment. This is where you start with an empty datapack."
          selected={autonomy === "draft"}
          onSelect={() => setAutonomy("draft")}
        />
      </div>

      <LabNote icon={ArrowUp}>
        <b className="font-semibold text-foreground">High quality context.</b> That is the trade:
        context is the price of autonomy, and it is paid once rather than per pull request.
      </LabNote>

      {/* Collapsed by default. It is the counter-argument, and a lesson that
          opens on its own worst case argues with itself. */}
      <Collapsible open={riskOpen} onOpenChange={setRiskOpen}>
        <div className="overflow-hidden rounded-lg border border-destructive/40 bg-destructive/[0.04]">
          <CollapsibleTrigger asChild>
            <button type="button" className="flex w-full items-center gap-2 px-4 py-3 text-left">
              <AlertTriangle className="h-4 w-4 shrink-0 text-destructive" aria-hidden />
              <span className="flex-1 text-sm font-semibold text-foreground">
                What handing over too early looks like
              </span>
              <span className="text-xs text-muted-foreground">{riskOpen ? "hide" : "show me"}</span>
              {riskOpen ? (
                <ChevronUp className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
              ) : (
                <ChevronDown className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
              )}
            </button>
          </CollapsibleTrigger>
          <CollapsibleContent>
            <div className="flex flex-col gap-4 border-t border-destructive/25 px-4 py-4">
              <p className="text-sm text-muted-foreground">
                We ran this same agent autonomously on 10 pull requests against a repo with no
                standards doc, no memories, nothing but the diff.
              </p>
              <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
                {EARLY_HANDOVER_STATS.map((stat) => (
                  <div key={stat.label}>
                    <p
                      className={cn(
                        "text-2xl font-bold tabular-nums",
                        stat.bad ? "text-destructive" : "text-foreground",
                      )}
                    >
                      {stat.value}
                    </p>
                    <p className="mt-0.5 text-xs leading-tight text-muted-foreground">
                      {stat.label}
                    </p>
                  </div>
                ))}
              </div>
              <p className="text-sm text-muted-foreground">
                Velocity without context is not velocity. It is throughput of work someone else has
                to undo. The gate is not there to slow you down; it is what makes letting go
                survivable.
              </p>
            </div>
          </CollapsibleContent>
        </div>
      </Collapsible>

      <p className="text-xs text-muted-foreground">
        Whichever you pick, the full trace is kept: the prompt, the rules retrieved, the reasoning,
        and who approved what. In this lab all three behave the same, because {agentDisplayName}{" "}
        reviews here rather than posting to a pull request, so there is nothing yet for the
        guardrail to hold back.
      </p>
    </div>
  );
}

function ChoiceCard({
  title,
  body,
  tag,
  selected,
  onSelect,
  locked,
}: {
  title: string;
  body: string;
  tag?: string;
  selected: boolean;
  onSelect: () => void;
  /** Shown but not choosable. The fastest rung is what the lab is arguing
   *  toward, not what it hands you after four minutes. */
  locked?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={locked}
      aria-pressed={selected}
      className={cn(
        "flex flex-col gap-1.5 rounded-lg border p-4 text-left transition-colors",
        selected ? "border-primary bg-primary/[0.04]" : "border-border bg-card",
        locked ? "opacity-60" : "hover:border-foreground/25",
      )}
    >
      <span className="flex flex-wrap items-center gap-2">
        <span
          className={cn(
            "flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
            selected ? "border-primary bg-primary text-primary-foreground" : "border-border",
          )}
          aria-hidden
        >
          {selected && <Check className="h-2.5 w-2.5" />}
        </span>
        <span className="text-sm font-semibold text-foreground">{title}</span>
        {tag && (
          <Badge variant={locked ? "secondary" : "success"} className="text-[10px]">
            {tag}
          </Badge>
        )}
        {locked && (
          <Badge variant="secondary" className="text-[10px]">
            not in this lab
          </Badge>
        )}
      </span>
      <span className="pl-6 text-xs leading-relaxed text-muted-foreground">{body}</span>
    </button>
  );
}
