// Three-up role picker (Viewer / Contributor / Maintainer) rendered as
// tiles with icon + label + one-line description. This replaces the
// plain <Select> that lived in InviteByEmailSection so the merged
// invite tab has a single, visually-rich role control instead of two
// side-by-side ways to pick the same value.
//
// Kept as its own file so a future "role picker somewhere else" (e.g.
// a per-invite role override on the pending-invites card) can reuse it
// without pulling in the whole invite orchestrator.
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  SHARE_INVITE_ROLES,
  type ShareInviteRoleId,
} from "@/features/datapacks/components/datapack-share/shareInviteRoles";

export type InviteRoleTilesProps = {
  value: ShareInviteRoleId;
  onChange: (id: ShareInviteRoleId) => void;
  disabled?: boolean;
  /** Optional label above the tile row. Provide when the tiles are the
   *  only role affordance visible (as they will be in the merged
   *  invite section); omit if the caller renders a heading of its own. */
  label?: string;
};

export function InviteRoleTiles({ value, onChange, disabled, label }: InviteRoleTilesProps) {
  return (
    <div className="space-y-2">
      {label ? (
        <span className="text-xs font-medium text-foreground">{label}</span>
      ) : null}
      <div className="grid gap-3 sm:grid-cols-3">
        {SHARE_INVITE_ROLES.map((r) => {
          const Icon = r.icon;
          const selected = value === r.id;
          return (
            <Button
              key={r.id}
              type="button"
              variant="outline"
              disabled={disabled}
              onClick={() => onChange(r.id)}
              className={cn(
                // Use a Button so keyboard nav / focus rings match the
                // rest of the app, but override the default padding /
                // alignment to render a tall tile.
                "flex h-auto flex-col items-start gap-2 rounded-xl p-4 text-left transition-colors",
                selected
                  ? "border-brand/60 bg-brand/10 shadow-[inset_0_0_0_1px_hsl(var(--brand)/0.35)]"
                  : "border-border bg-muted/20 hover:border-muted-foreground/30 hover:bg-muted/30",
                disabled && "pointer-events-none opacity-60",
              )}
              // Role tile behaves as a radio; TabsList-style semantics
              // aren't a fit because these can be interspersed with
              // other buttons in the same section.
              aria-pressed={selected}
            >
              <Icon
                className={cn(
                  "h-5 w-5 shrink-0",
                  selected ? "text-brand" : "text-muted-foreground",
                )}
                aria-hidden
              />
              <span className="text-sm font-semibold text-foreground">{r.label}</span>
              <span className="whitespace-normal text-xs leading-snug text-muted-foreground">
                {r.description}
              </span>
            </Button>
          );
        })}
      </div>
    </div>
  );
}
