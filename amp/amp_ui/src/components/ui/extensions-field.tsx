// ExtensionsField — pick externalized Postgres extensions. A multi-select card
// grid in the EngineField idiom; presentation-only, emits the selected catalog
// names. Options that are already enabled render checked and locked (the API
// unions, and there is no removal path).
//
// The catalog is bundled rather than fetched: the controller exposes no list
// endpoint. It is GENERATED from deploy/extensions/extensions.yaml (the build
// manifest, and the source of truth for which extensions exist at what version)
// by scripts/gen-extensions.py, so the picker cannot drift from what is built —
// run `npm run gen:extensions` after editing the manifest.

import { Check } from "lucide-react";
import catalog from "@/config/extensions.json";
import { cn } from "@/lib/utils";

export type ExtensionOption = {
  /** Catalog name — what the API takes. */
  name: string;
  version: string;
  description: string;
  /** Needs shared_preload_libraries, so enabling it restarts the postmaster. */
  preload?: boolean;
};

export const AMP_EXTENSIONS: ExtensionOption[] = catalog.extensions;

export function ExtensionsField({
  value,
  onChange,
  enabled = [],
  options = AMP_EXTENSIONS,
  showRestartTag = true,
  className,
}: {
  value: string[];
  onChange: (names: string[]) => void;
  /** Already enabled on the target — shown checked and not togglable. */
  enabled?: string[];
  options?: ExtensionOption[];
  /** Flag the preload extensions as restarting the compute. Off where there is
   *  no compute yet to restart (creating a cluster). */
  showRestartTag?: boolean;
  className?: string;
}) {
  function toggle(name: string) {
    onChange(
      value.includes(name) ? value.filter((n) => n !== name) : [...value, name],
    );
  }

  return (
    <div className={cn("grid grid-cols-1 gap-2 sm:grid-cols-2", className)}>
      {options.map((o) => {
        const locked = enabled.includes(o.name);
        const on = locked || value.includes(o.name);
        return (
          <button
            key={o.name}
            type="button"
            role="checkbox"
            aria-checked={on}
            aria-label={o.name}
            disabled={locked}
            onClick={() => !locked && toggle(o.name)}
            className={cn(
              "rounded-lg border border-border bg-background p-3 text-left transition-colors",
              !locked && "hover:border-muted-foreground/40",
              on && "border-brand bg-brand/10 ring-1 ring-brand",
              locked && "cursor-not-allowed opacity-60",
            )}
          >
            <div className="flex items-center gap-1.5 text-sm font-semibold">
              <span
                className={cn(
                  "flex h-4 w-4 shrink-0 items-center justify-center rounded border",
                  on ? "border-brand bg-brand text-brand-foreground" : "border-border",
                )}
              >
                {on && <Check className="h-3 w-3" strokeWidth={3} />}
              </span>
              <span className="font-mono">{o.name}</span>
              <span className="font-mono text-xs font-normal text-muted-foreground">
                {o.version}
              </span>
            </div>
            <p className="mt-1 text-xs text-muted-foreground">{o.description}</p>
            {(locked || (o.preload && showRestartTag)) && (
              <div className="mt-1.5 flex flex-wrap gap-1">
                {locked && <Tag>Enabled</Tag>}
                {o.preload && showRestartTag && <Tag>Needs restart</Tag>}
              </div>
            )}
          </button>
        );
      })}
    </div>
  );
}

function Tag({ children }: { children: React.ReactNode }) {
  return (
    <span className="inline-block rounded-full bg-muted px-2 py-0.5 text-[10px] font-medium text-muted-foreground">
      {children}
    </span>
  );
}
