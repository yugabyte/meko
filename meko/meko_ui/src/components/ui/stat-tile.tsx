// StatTile — small "big number under an icon + label" card used across
// detail pages (Artifacts, and reusable elsewhere). Presentation-only:
// the caller supplies the icon element (already coloured) and the
// value node so the tile stays flexible about content types (numbers,
// relative-time strings, JSX badges).

import type { ReactNode } from "react";

type Props = {
  icon: ReactNode;
  label: string;
  value: ReactNode;
};

export function StatTile({ icon, label, value }: Props) {
  return (
    <div className="rounded-xl border border-border bg-card p-4">
      <div className="flex items-center gap-2 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
        {icon}
        {label}
      </div>
      <p className="mt-2 text-2xl font-semibold text-foreground">{value}</p>
    </div>
  );
}
