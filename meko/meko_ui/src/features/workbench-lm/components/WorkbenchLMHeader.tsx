// Header content for the WorkbenchLM page: title + a datapack picker
// across every pack the user can chat against (own, maintainer, or
// contributor — viewer is excluded because chat writes memories).
// Rendered inside `AppWorkspaceHeader`'s `leading` slot.

import { useMemo } from "react";
import { PlayCircle } from "lucide-react";
import { Combobox, type ComboboxOption } from "@/components/ui/combobox";
import { cn } from "@/lib/utils";
import type { Datapack } from "@/lib/api/mekoApi";
import { formatDatapackShareGrantCamelCase } from "@/lib/datapackShareGrants";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { FREE_TIER_LIMITS, FREE_TIER_QUOTA_ENABLED, isFreeTier } from "@/lib/freeTier";

// Compact integer formatter — 1234 → "1.2K", 10_000_000 → "10M".
// Single shared instance so we don't allocate per render.
const COMPACT_NUMBER = new Intl.NumberFormat("en", {
  notation: "compact",
  maximumFractionDigits: 1,
});

type Props = {
  /** Datapacks the signed-in user can use in the WorkbenchLM: own + shared
   *  with maintainer / contributor grant. Pre-filtered by the parent via
   *  `isDatapackGrantEligibleForWorkbenchLM`. */
  accessibleDatapacks: Datapack[];
  /** ID of the currently-selected datapack. ID rather than name so two
   *  same-named datapacks (e.g. an owned and a shared "hello") stay
   *  distinguishable — the Combobox is keyed by ID too (CommandItem
   *  value={option.value}), so keyboard arrows walk distinct rows even
   *  when their visible labels collide. */
  selectedDatapackId: string | null;
  /** Fired with the ID of the picked datapack. The parent maps ID back
   *  to a name for downstream API calls. */
  onSelectDatapack: (datapackId: string) => void;
  loading: boolean;
};

export function WorkbenchLMHeader({
  accessibleDatapacks,
  selectedDatapackId,
  onSelectDatapack,
  loading,
}: Props) {
  // Look the name up from the active record so the trigger's tooltip
  // still shows a friendly string. Drives the sublabel under the title
  // too. Returns null when the ID is stale (parent's auto-select will
  // fix it on the next render).
  const selectedDatapackName =
    accessibleDatapacks.find((dp) => dp.datapack_id === selectedDatapackId)?.datapack_name ??
    null;

  // Map every accessible datapack to a Combobox option. Role goes in the
  // label as a text suffix (e.g. "name · OWNER") so Combobox's search
  // matches on role too — the previous visual grant chip is inlined,
  // keeping the "yours vs shared" cue without extending the Combobox
  // API with a per-row suffix slot. Upper-cased explicitly here because
  // the chip's old `uppercase` CSS class is gone with the chip.
  // Combobox keys cmdk items by option.value (datapack_id), so two
  // same-named datapacks don't collapse into one keyboard target.
  const datapackOptions = useMemo<ComboboxOption[]>(
    () =>
      accessibleDatapacks.map((dp) => ({
        value: dp.datapack_id,
        label: dp.grant
          ? `${dp.datapack_name} · ${formatDatapackShareGrantCamelCase(dp.grant).toUpperCase()}`
          : dp.datapack_name,
      })),
    [accessibleDatapacks],
  );

  // WorkbenchLM token-burn usage indicator. Reads from the same shared
  // react-query cache that drives the chat-composer gate, so this is a
  // free read. Hidden until the first /auth/me/usage response so the
  // header doesn't flicker "—/10M" → "1.2K/10M" on mount. Also hidden
  // for paid tiers entirely — Pro / Enterprise have no token cap, so
  // the "x / 10M" framing would be misleading.
  const { data: accountUsage } = useAccountUsage();
  const { data: currentUser } = useCurrentUser();
  const showTokenBurn =
    FREE_TIER_QUOTA_ENABLED && !!accountUsage && isFreeTier(currentUser?.user_type);
  const tokenBurnLabel = showTokenBurn
    ? `${COMPACT_NUMBER.format(accountUsage.token_burn)} / ${COMPACT_NUMBER.format(FREE_TIER_LIMITS.tokenBurn)}`
    : null;
  const tokenBurnAtCap = showTokenBurn
    ? accountUsage.token_burn >= FREE_TIER_LIMITS.tokenBurn
    : false;

  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <div className="flex min-w-0 items-center gap-2.5">
        <PlayCircle className="h-4 w-4 shrink-0 text-brand" aria-hidden />
        <h1 className="text-lg font-semibold text-foreground">WorkbenchLM</h1>
        <Combobox
          options={datapackOptions}
          value={selectedDatapackId ?? ""}
          onChange={onSelectDatapack}
          disabled={loading || accessibleDatapacks.length === 0}
          placeholder={
            loading
              ? "Loading datapacks…"
              : accessibleDatapacks.length === 0
                ? "No datapacks available"
                : "Select a datapack"
          }
          searchPlaceholder="Search datapacks…"
          emptyText="No matching datapacks."
          className="h-8 w-[260px] border-border/70 bg-card text-sm"
        />
      </div>
      {/* One-line "what this thing is" summary — kept here in the header
          rather than only on first visit so users who jump straight into
          WorkbenchLM (from Onboarding's CTA or the sidebar) have an
          always-on reminder that this is a *demo surface*, not Meko
          itself. */}
      <p className="text-xs italic text-muted-foreground/80">
        A sandbox to see Meko save and recall memory. Meko itself is the data layer your own AI
        tools connect to.
      </p>
      <p className="truncate text-xs text-muted-foreground" title={selectedDatapackName ?? undefined}>
        {selectedDatapackName ? (
          <>
            Datapack:{" "}
            <span className="font-medium text-foreground">{selectedDatapackName}</span>
          </>
        ) : (
          "Pick a datapack to start exploring."
        )}
      </p>
      {tokenBurnLabel ? (
        <p className="text-xs text-muted-foreground">
          Tokens burned:{" "}
          <span
            className={cn(
              "font-mono font-medium",
              tokenBurnAtCap ? "text-amber-700 dark:text-amber-300" : "text-foreground",
            )}
          >
            {tokenBurnLabel}
          </span>
        </p>
      ) : null}
    </div>
  );
}
