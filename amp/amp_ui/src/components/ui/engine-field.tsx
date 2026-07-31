// EngineField — pick a database engine. Only enabled engines are selectable;
// the rest render greyed out with a "Coming soon" tag (roadmap surface).
// Presentation-only; emits the selected engine id. Theme-token styled.

import { cn } from "@/lib/utils";

export type EngineOption = { id: string; label: string; sub?: string; comingSoon?: boolean };

// PostgreSQL is the only engine AMP supports today; the rest are roadmap.
export const DB_ENGINES: EngineOption[] = [
  { id: "postgres", label: "PostgreSQL", sub: "18" },
  { id: "aidb", label: "AI Database", comingSoon: true },
  { id: "oracle", label: "Oracle", comingSoon: true },
  { id: "sqlserver", label: "SQL Server", comingSoon: true },
  { id: "documentdb", label: "DocumentDB", comingSoon: true },
];

export function EngineField({
  value,
  onChange,
  options = DB_ENGINES,
  className,
}: {
  value: string;
  onChange: (id: string) => void;
  options?: EngineOption[];
  className?: string;
}) {
  return (
    <div className={cn("grid grid-cols-2 gap-2 sm:grid-cols-3", className)}>
      {options.map((o) => {
        const disabled = Boolean(o.comingSoon);
        const on = value === o.id && !disabled;
        return (
          <button
            key={o.id}
            type="button"
            disabled={disabled}
            aria-pressed={on}
            onClick={() => !disabled && onChange(o.id)}
            className={cn(
              "rounded-lg border border-border bg-background p-3 text-left transition-colors",
              !disabled && "hover:border-muted-foreground/40",
              on && "border-brand bg-brand/10 ring-1 ring-brand",
              disabled && "cursor-not-allowed opacity-60",
            )}
          >
            <div className="flex items-center gap-1.5 text-sm font-semibold">
              {o.label}
              {o.sub && !disabled ? (
                <span className="font-mono text-xs font-normal text-muted-foreground">{o.sub}</span>
              ) : null}
            </div>
            {disabled ? (
              <span className="mt-1.5 inline-block rounded-full bg-muted px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                Coming soon
              </span>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}
