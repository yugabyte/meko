// One selectable layout in the new-collection picker: its label, what it is for,
// and the folders it will create. Previewing the folder names is the point —
// it is the only thing that distinguishes one layout from another.

import { Check, Folder } from "lucide-react";
import { cn } from "@/lib/utils";
import type { CollectionTheme } from "@/lib/api/collectionsApi";

export function CollectionThemeOption({
  theme,
  selected,
  onSelect,
}: {
  theme: CollectionTheme;
  selected: boolean;
  onSelect: (key: string) => void;
}) {
  return (
    <label
      className={cn(
        "flex cursor-pointer items-start gap-3 rounded-lg border p-3 transition-colors",
        selected
          ? "border-brand/50 bg-brand/5"
          : "border-border bg-background hover:border-brand/30 hover:bg-muted/40",
      )}
    >
      <input
        type="radio"
        name="collection-theme"
        value={theme.key}
        checked={selected}
        onChange={() => onSelect(theme.key)}
        className="sr-only"
      />
      <span
        className={cn(
          "mt-0.5 flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
          selected ? "border-brand bg-brand text-primary-foreground" : "border-border",
        )}
        aria-hidden
      >
        {selected ? <Check className="h-3 w-3" /> : null}
      </span>

      <span className="min-w-0 flex-1">
        <span className="block text-sm font-medium text-foreground">{theme.label}</span>
        <span className="mt-0.5 block text-xs leading-relaxed text-muted-foreground">
          {theme.description}
        </span>
        <span className="mt-2 flex flex-wrap gap-1.5">
          {theme.folders.map((folder) => (
            <span
              key={folder}
              className="inline-flex items-center gap-1 rounded-md bg-muted px-1.5 py-0.5 text-xs text-muted-foreground"
            >
              <Folder className="h-3 w-3" aria-hidden />
              {folder}
            </span>
          ))}
        </span>
      </span>
    </label>
  );
}
