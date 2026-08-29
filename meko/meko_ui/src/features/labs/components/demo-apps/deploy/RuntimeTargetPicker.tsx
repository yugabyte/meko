// Runtime-target picker: 3 cards, only AgentCore is clickable today. Vercel
// and Lyzr are visible-but-disabled "coming soon" cards, following the same
// disabled+Tooltip idiom as CollectiveLearningPipeline.tsx's "Tune
// thresholds" button — a real card so the roadmap is legible, not just an
// absent option.

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

export type RuntimeTargetId = "bedrock-agentcore" | "vercel" | "lyzr";

const RUNTIME_TARGETS: { id: RuntimeTargetId; label: string; description: string; enabled: boolean }[] = [
  {
    id: "bedrock-agentcore",
    label: "AWS Bedrock AgentCore",
    description: "Deploy as a managed AgentCore Runtime container.",
    enabled: true,
  },
  {
    id: "vercel",
    label: "Vercel",
    description: "Deploy as a serverless function.",
    enabled: false,
  },
  {
    id: "lyzr",
    label: "Lyzr",
    description: "Deploy to Lyzr's agent hosting.",
    enabled: false,
  },
];

export function RuntimeTargetPicker({
  selected,
  onSelect,
}: {
  selected: RuntimeTargetId | null;
  onSelect: (id: RuntimeTargetId) => void;
}) {
  return (
    <TooltipProvider delayDuration={200}>
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
        {RUNTIME_TARGETS.map((target) =>
          target.enabled ? (
            <Card
              key={target.id}
              role="button"
              tabIndex={0}
              onClick={() => onSelect(target.id)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") onSelect(target.id);
              }}
              className={cn(
                "cursor-pointer p-4 transition-colors hover:bg-muted/30",
                selected === target.id && "border-primary ring-2 ring-primary/30",
              )}
            >
              <p className="text-sm font-medium">{target.label}</p>
              <p className="mt-1 text-xs text-muted-foreground">{target.description}</p>
            </Card>
          ) : (
            <Tooltip key={target.id}>
              <TooltipTrigger asChild>
                <span className="inline-flex">
                  <Card
                    className="pointer-events-none w-full select-none p-4 opacity-70"
                    aria-disabled="true"
                  >
                    <div className="flex items-center justify-between gap-2">
                      <p className="text-sm font-medium">{target.label}</p>
                      <Badge variant="outline">Coming soon</Badge>
                    </div>
                    <p className="mt-1 text-xs text-muted-foreground">{target.description}</p>
                  </Card>
                </span>
              </TooltipTrigger>
              <TooltipContent side="top">Coming soon</TooltipContent>
            </Tooltip>
          ),
        )}
      </div>
    </TooltipProvider>
  );
}
