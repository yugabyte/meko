import { badgeVariants } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_OWNER,
  DATAPACK_SHARE_GRANT_VIEWER,
  type DatapackShareGrant,
  formatDatapackShareGrantCamelCase,
} from "@/lib/datapackShareGrants";

function grantBadgeAccent(grant: DatapackShareGrant): string {
  switch (grant) {
    case DATAPACK_SHARE_GRANT_OWNER:
      return "border-blue-500/35 bg-blue-500/10 text-blue-200";
    case DATAPACK_SHARE_GRANT_MAINTAINER:
      return "border-violet-500/35 bg-violet-500/12 text-violet-200";
    case DATAPACK_SHARE_GRANT_CONTRIBUTOR:
      return "border-amber-500/30 bg-amber-500/10 text-amber-100";
    case DATAPACK_SHARE_GRANT_VIEWER:
      return "border-border bg-muted/50 text-muted-foreground";
  }
}

type DatapackGrantBadgeProps = {
  grant: DatapackShareGrant;
  className?: string;
};

/**
 * Compact grant pill for list rows (share picker, etc.). Uses {@link badgeVariants} like {@link Badge}
 * but renders a `span` so it can sit inside `button` rows. Colors align with owned (blue) vs shared (violet) cues elsewhere.
 */
export function DatapackGrantBadge({ grant, className }: DatapackGrantBadgeProps) {
  return (
    <span
      className={cn(
        badgeVariants({ variant: "outline" }),
        "shrink-0 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide",
        grantBadgeAccent(grant),
        className,
      )}
    >
      {formatDatapackShareGrantCamelCase(grant)}
    </span>
  );
}
