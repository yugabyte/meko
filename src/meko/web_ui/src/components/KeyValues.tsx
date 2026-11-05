// Labels and their values in two columns, for details such as a call's or the server's.

import type { ReactNode } from "react";

export function KeyValues({ rows }: { rows: [string, ReactNode][] }) {
  return (
    <dl className="grid grid-cols-[8rem_1fr] gap-y-1.5 text-sm">
      {rows.map(([k, v]) => (
        <div key={k} className="contents">
          <dt className="text-muted-foreground">{k}</dt>
          <dd className="min-w-0 break-all">{v}</dd>
        </div>
      ))}
    </dl>
  );
}
