// ExtensionsField — pick externalized Postgres extensions. A multi-select card
// grid in the EngineField idiom; presentation-only, emits the selected catalog
// names. Options that are already enabled render checked and locked (the API
// unions, and there is no removal path).
//
// The catalog comes from the controller (GET /extensions, via useExtensionOptions
// below), which is what makes an image an admin registered selectable without
// shipping a new UI. The bundled list generated from the build manifest
// (scripts/gen-extensions.py) is kept only as the first paint and the fallback
// when that request has not landed or failed — it can only ever contain the
// built-ins, so a registered extension would be missing from it.

import { Check } from "lucide-react";
import catalog from "@/config/extensions.json";
import { useExtensions } from "@/lib/api/queries";
import { cn } from "@/lib/utils";

export type ExtensionOption = {
  /** Catalog name — what the API takes. */
  name: string;
  /** Both optional: the controller reports them only when the entry carries
   *  them, and a registered extension need not declare either. */
  version?: string;
  description?: string;
  /** Needs shared_preload_libraries, so enabling it restarts the postmaster. */
  preload?: boolean;
  /** False for an image an admin registered. Undefined in the bundled list,
   *  which predates the distinction and holds only built-ins. */
  builtin?: boolean;
};

/** The extensions this UI was built against. Not the catalog — a controller
 *  serves more than this as soon as an admin registers anything. */
export const BUNDLED_EXTENSIONS: ExtensionOption[] = catalog.extensions;

/** The catalog to offer, live from the controller, falling back to the bundled
 *  built-ins while the request is in flight or if it fails. Returned alongside
 *  the query state so a caller can say "catalog unavailable" rather than
 *  silently offering a short list. */
export function useExtensionOptions(): {
  options: ExtensionOption[];
  isLoading: boolean;
  isError: boolean;
} {
  const { data, isLoading, isError } = useExtensions();
  return {
    options: data && data.length > 0 ? data : BUNDLED_EXTENSIONS,
    isLoading,
    isError,
  };
}

export function ExtensionsField({
  value,
  onChange,
  enabled = [],
  options = BUNDLED_EXTENSIONS,
  showRestartTag = true,
  className,
}: {
  value: string[];
  onChange: (names: string[]) => void;
  /** Already enabled on the target — shown checked and not togglable. */
  enabled?: string[];
  /** The catalog to offer. Callers pass useExtensionOptions().options so a
   *  registered extension shows up; the default is the bundled built-ins. */
  options?: ExtensionOption[];
  /** Flag the preload extensions as restarting the compute. Off where there is
   *  no compute yet to restart (creating a cluster). */
  showRestartTag?: boolean;
  className?: string;
}) {
  function toggle(name: string) {
    onChange(value.includes(name) ? value.filter((n) => n !== name) : [...value, name]);
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
