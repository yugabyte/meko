// The nested datapack rows under the sidebar's Datapacks entry, with the
// filter that makes a long list usable. Split out of Sidebar.tsx, which had
// grown past the point where one more stateful block belonged in it.

import { useMemo, useState } from "react";
import { NavLink, useLocation } from "react-router-dom";
import { Search, Users } from "lucide-react";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { datapackDetailPath } from "@/app/routes/paths";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";
import type { Datapack } from "@/lib/api/mekoApi";
import {
  DATAPACK_FILTER_PLACEHOLDER,
  DATAPACK_FILTER_NO_MATCHES,
} from "@/features/layout/sidebarCopy";

/**
 * Cap the nested "Datapacks" list at this many rows before it starts to
 * scroll internally. Without a cap, a user with 30+ datapacks pushes
 * everything below — Observe, Feedback, Sign out — off-screen unless
 * they scroll the whole sidebar.
 */
const MAX_VISIBLE_DATAPACKS = 7;

/**
 * Approximate height of one rendered row in the datapacks list.
 * `py-1.5` adds 12px of vertical padding; `text-xs` text + the
 * `gap-0.5` between siblings round the per-row footprint to ~28px.
 * Driving the cap off this constant means changing the row chrome
 * automatically rescopes the scroll threshold.
 */
const DATAPACK_ROW_HEIGHT_PX = 28;

export function SidebarDatapackList({
  datapacks,
  activeDatapackId,
}: {
  datapacks: Datapack[];
  /** From the `datapack_id` query param; disambiguates stale links. */
  activeDatapackId: string;
}) {
  const location = useLocation();
  const [filter, setFilter] = useState("");

  // The filter appears exactly when the list stops fitting, which is the
  // point at which scrolling to find a name starts costing something. Below
  // that it would be a permanent input above seven rows you can already see.
  const showFilter = datapacks.length > MAX_VISIBLE_DATAPACKS;

  const visible = useMemo(() => {
    const q = filter.trim().toLowerCase();
    if (!q || !showFilter) return datapacks;
    return datapacks.filter((dp) => dp.datapack_name.toLowerCase().includes(q));
  }, [datapacks, filter, showFilter]);

  return (
    <div className="ml-4 mt-0.5 flex flex-col gap-1 border-l border-white/10 pl-2">
      {showFilter ? (
        <div className="relative pr-1">
          <Search
            className="pointer-events-none absolute left-2 top-1/2 h-3 w-3 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            type="search"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder={DATAPACK_FILTER_PLACEHOLDER}
            aria-label={DATAPACK_FILTER_PLACEHOLDER}
            className="h-7 pl-7 text-xs"
          />
        </div>
      ) : null}

      {/* Only when a filter is actually narrowing something. An account with
          no datapacks, and the first render before they load, both reach here
          with an empty list and must show nothing, the way they did before
          this list had a filter at all. */}
      {visible.length === 0 ? (
        filter.trim() ? (
          <p className="px-2 py-1.5 text-xs text-muted-foreground">{DATAPACK_FILTER_NO_MATCHES}</p>
        ) : null
      ) : (
        <div
          className="flex flex-col gap-0.5 overflow-y-auto"
          style={{ maxHeight: MAX_VISIBLE_DATAPACKS * DATAPACK_ROW_HEIGHT_PX }}
        >
          {visible.map((dp) => (
            <NavLink
              key={dp.datapack_id}
              to={datapackDetailPath(dp.datapack_id)}
              title={dp.datapack_name}
              className={() => {
                const basePath = datapackDetailPath(dp.datapack_id);
                const onDatapackSubroute =
                  location.pathname === basePath || location.pathname.startsWith(`${basePath}/`);
                // Keep the datapack highlighted whenever the current route is
                // under that datapack path. If a datapack_id query is present,
                // also require id match to avoid ambiguity on stale links.
                const idMatches = !activeDatapackId || activeDatapackId === dp.datapack_id;
                const isActive = onDatapackSubroute && idMatches;
                return cn(
                  "flex items-center rounded-md py-1.5 pl-2 pr-2 text-xs font-medium transition-colors min-w-0",
                  isActive
                    ? "bg-foreground/10 text-foreground"
                    : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
                );
              }}
            >
              {/*
                `flex-1` on the name span makes it occupy all the
                free horizontal space inside the row. That pushes
                the share icon flush to the right edge regardless
                of name length — without it, short names left the
                icon floating mid-row right next to the text.
              */}
              <span className="min-w-0 flex-1 truncate">{dp.datapack_name}</span>
              {!isDatapackListGrantOwner(dp) ? (
                <Users
                  className="ml-1.5 h-3 w-3 shrink-0 text-purple-700 dark:text-purple-300/80"
                  aria-label="Shared with you"
                />
              ) : null}
            </NavLink>
          ))}
        </div>
      )}
    </div>
  );
}
