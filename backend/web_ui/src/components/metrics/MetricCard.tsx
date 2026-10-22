// The frame every Analytics card uses: a title, a subtitle, and its body or an empty state.

import type { ReactNode } from "react";
import { Card, cx } from "../ui";

export function MetricCard({
  title,
  subtitle,
  empty,
  className,
  children,
}: {
  title: string;
  subtitle?: string;
  // When set, shown instead of the body: [heading, hint].
  empty?: [string, string];
  className?: string;
  children?: ReactNode;
}) {
  return (
    <Card className={cx("flex flex-col p-5", className)}>
      <h3 className="font-semibold">{title}</h3>
      {subtitle && <p className="mt-0.5 text-xs text-muted-foreground">{subtitle}</p>}
      {empty ? (
        <div className="flex flex-1 flex-col items-center justify-center py-8 text-center">
          <p className="text-sm font-medium">{empty[0]}</p>
          <p className="mt-1 max-w-[16rem] text-xs text-muted-foreground">{empty[1]}</p>
        </div>
      ) : (
        <div className="mt-4 flex-1">{children}</div>
      )}
    </Card>
  );
}

// A big number with its label under it.
export function BigStat({ value, label, unit }: { value: string; label?: string; unit?: string }) {
  return (
    <div>
      <div className="text-3xl font-semibold tabular-nums">
        {value}
        {unit && <span className="ml-1 text-base font-normal text-muted-foreground">{unit}</span>}
      </div>
      {label && <div className="mt-0.5 text-xs text-muted-foreground">{label}</div>}
    </div>
  );
}
