// Search + role-filter toolbar for the Members roster.
//
// Filter state is owned by MembersSection (so tab switches don't reset
// it); this component is a controlled view. The Export CSV button that
// used to sit next to the role filter was removed — the roster is
// already visible, the filter is for finding a specific member, and
// mixing a "download" affordance on a management toolbar read as
// clutter. If audit needs an export in future, add it to a dedicated
// admin surface, not here.
import { Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  EMPTY_SHARE_MEMBER_FILTER,
  SHARE_MEMBER_ROLE_FILTER_OPTIONS,
  isShareMemberFilterActive,
  type ShareMemberFilter,
  type ShareMemberRoleFilter,
} from "@/features/datapacks/components/datapack-share/shareMemberFilter";

export type MembersToolbarProps = {
  filter: ShareMemberFilter;
  onFilterChange: (next: ShareMemberFilter) => void;
  /** Number of rows currently rendered by the filter — used only for the
   *  "Showing X of Y" line, not passed to any export. */
  visibleCount: number;
  totalCount: number;
};

export function MembersToolbar({
  filter,
  onFilterChange,
  visibleCount,
  totalCount,
}: MembersToolbarProps) {
  const filtering = isShareMemberFilterActive(filter);
  return (
    <div className="space-y-2">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
        <div className="relative min-w-0 flex-1">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            type="search"
            value={filter.query}
            aria-label="Search members by name, email, domain, or role"
            placeholder="Search name, email, domain (@company.com), or role"
            className="h-9 bg-background/80 pl-8 text-sm"
            onChange={(e) => onFilterChange({ ...filter, query: e.target.value })}
          />
        </div>
        <Select
          value={filter.role}
          onValueChange={(v) => onFilterChange({ ...filter, role: v as ShareMemberRoleFilter })}
        >
          <SelectTrigger
            aria-label="Filter members by role"
            className="h-9 w-full bg-background/80 text-xs sm:w-[150px]"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {SHARE_MEMBER_ROLE_FILTER_OPTIONS.map((o) => (
              <SelectItem key={o.id} value={o.id}>
                {o.label}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      {filtering ? (
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <span aria-live="polite">
            Showing {visibleCount} of {totalCount}
          </span>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-6 px-2 text-xs"
            onClick={() => onFilterChange(EMPTY_SHARE_MEMBER_FILTER)}
          >
            Clear filters
          </Button>
        </div>
      ) : null}
    </div>
  );
}
