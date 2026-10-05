// The trailing "add one" card at the end of a card grid.
//
// Shared: the Datapacks grid and the Collections grid both end with one, and a
// third grid would want the same. Was NewDatapackPlaceholderCard, local to
// datapacks, until Collections needed the identical affordance.

import { Plus } from "lucide-react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export function CreateTile({
  title,
  description,
  onClick,
  className,
}: {
  title: string;
  description: string;
  onClick: () => void;
  className?: string;
}) {
  return (
    <Card
      role="button"
      tabIndex={0}
      className={cn(
        "group flex min-h-[12rem] flex-col items-center justify-center gap-3 border border-dashed border-border/70 bg-transparent p-6 shadow-none",
        "cursor-pointer transition-colors hover:border-brand/50 hover:bg-brand/[0.04]",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand focus-visible:ring-offset-2 focus-visible:ring-offset-card",
        className,
      )}
      onClick={onClick}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClick();
        }
      }}
    >
      <div
        className={cn(
          "flex h-10 w-10 items-center justify-center rounded-lg border border-muted-foreground/45",
          "transition-colors group-hover:border-brand/60 group-hover:bg-brand/10",
        )}
        aria-hidden
      >
        <Plus className="h-5 w-5 text-muted-foreground transition-colors group-hover:text-brand" />
      </div>
      <div className="space-y-1 text-center">
        <p className="text-base font-semibold text-foreground">{title}</p>
        <p className="max-w-[14rem] text-xs leading-relaxed text-muted-foreground">{description}</p>
      </div>
    </Card>
  );
}
