// EngineField — pick a database engine. An engine is a named bundle of
// externalized extensions the controller seeds onto the new cluster's default
// branch (compute/architecture/extensions.md); picking one is how a user gets
// "AI Database" without knowing it means pgvector + age + pg_cron.
//
// The supported engines are GENERATED from deploy/extensions/extensions.yaml
// (see extensions-field.tsx) so this list, the controller's engines.go, and the
// build manifest cannot drift. Engines the product does not implement yet are
// listed here as roadmap tiles and render disabled.

import catalog from "@/config/extensions.json";
import { cn } from "@/lib/utils";

export type EngineOption = {
  id: string;
  label: string;
  sub?: string;
  /** Catalog names the engine seeds. Empty for plain Postgres. */
  extensions?: string[];
  comingSoon?: boolean;
};

/** Engines the controller accepts, from the generated catalog. */
const SUPPORTED: EngineOption[] = catalog.engines.map((e) => ({
  id: e.name,
  label: e.label,
  sub: e.name === "postgres" ? "18" : undefined,
  extensions: e.extensions,
}));

/** Roadmap engines — no controller support, so they stay unselectable. */
const ROADMAP: EngineOption[] = [
  { id: "sqlserver", label: "SQL Server", comingSoon: true },
  { id: "documentdb", label: "DocumentDB", comingSoon: true },
];

export const DB_ENGINES: EngineOption[] = [...SUPPORTED, ...ROADMAP];

/** Display name for a stored engine. Rows written before the engine column
 *  existed carry none, so fall back to naming the Postgres version. */
export function engineLabel(engine: string | undefined, pgVersion?: number): string {
  const known = DB_ENGINES.find((e) => e.id === engine);
  if (known) return known.id === "postgres" ? `PostgreSQL ${pgVersion ?? 18}` : known.label;
  return `PostgreSQL ${pgVersion ?? 18}`;
}

/** The extensions an engine seeds; empty for an unknown or bundle-less engine. */
export function engineExtensions(id: string): string[] {
  return DB_ENGINES.find((e) => e.id === id)?.extensions ?? [];
}

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
            {!disabled && o.extensions && o.extensions.length > 0 ? (
              <p className="mt-1 font-mono text-[10px] text-muted-foreground">
                {o.extensions.join(" · ")}
              </p>
            ) : null}
          </button>
        );
      })}
    </div>
  );
}
