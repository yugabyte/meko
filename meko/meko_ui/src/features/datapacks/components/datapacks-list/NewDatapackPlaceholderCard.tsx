// Trailing "add one" card in the Datapacks grid. Extracted from
// DatapacksTilesSection alongside DatapackTile so that file stays a container.

import { Plus } from "lucide-react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export function NewDatapackPlaceholderCard({ onClick }: { onClick: () => void }) {
  return (
    <Card
      role="button"
      tabIndex={0}
      className={cn(
        "flex min-h-[12rem] flex-col items-center justify-center gap-3 border border-dashed border-border/70 bg-transparent p-6 shadow-none transition-colors",
        "cursor-pointer hover:border-muted-foreground/45 hover:bg-white/[0.03]",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand focus-visible:ring-offset-2 focus-visible:ring-offset-card",
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
        className="flex h-10 w-10 items-center justify-center rounded-lg border border-muted-foreground/45"
        aria-hidden
      >
        <Plus className="h-5 w-5 text-muted-foreground" />
      </div>
      <div className="space-y-1 text-center">
        <p className="text-base font-semibold text-foreground">New datapack</p>
        <p className="max-w-[14rem] text-xs leading-relaxed text-muted-foreground">
          Connect an existing collection or start from scratch
        </p>
      </div>
    </Card>
  );
}
