// A neutral aside inside a lesson — a pointer, not a status. WarningBanner
// covers the amber/green ends of that scale; this is the middle, for the
// "look over there" and "here's what just happened" lines that carry no
// severity. Shared by the Chapter 1 lessons.

import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";

export function LabNote({
  icon: Icon,
  children,
  className,
}: {
  icon: LucideIcon;
  children: React.ReactNode;
  className?: string;
}) {
  return (
    <div
      className={cn(
        "flex items-start gap-2.5 rounded-md border border-border bg-muted/30 px-4 py-3 text-sm text-muted-foreground",
        className,
      )}
    >
      <Icon className="mt-0.5 h-4 w-4 shrink-0 text-primary" aria-hidden />
      <div className="min-w-0 flex-1 leading-relaxed">{children}</div>
    </div>
  );
}
