/**
 * Horizontal tab strip for switching datapack sections (Conversations, Memory, Knowledge, Learnings).
 */
import { cn } from "@/lib/utils";
// types.ts: tab id union + ordered labels rendered as buttons here (same list as the detail page).
import type { DatapackDetailTab } from "./types";
import { DATAPACK_DETAIL_TAB_ITEMS } from "./types";

type Props = {
  activeTab: DatapackDetailTab;
  onTabChange: (tab: DatapackDetailTab) => void;
  className?: string;
};

export function DatapackDetailTabStrip({ activeTab, onTabChange, className }: Props) {
  return (
    <div className={cn("w-full min-w-0", className)}>
      <div className="flex w-full min-w-0 gap-1 overflow-x-auto border-b border-border pb-px">
        {DATAPACK_DETAIL_TAB_ITEMS.map((tab) => (
          <button
            key={tab.id}
            type="button"
            onClick={() => onTabChange(tab.id)}
            className={cn(
              "shrink-0 rounded-t-md px-3 py-2 text-sm font-medium transition-colors",
              activeTab === tab.id
                ? "bg-[#1C2333] text-white border border-b-0 border-border -mb-px"
                : "text-muted-foreground hover:text-foreground hover:bg-muted/30",
            )}
          >
            {tab.label}
          </button>
        ))}
      </div>
    </div>
  );
}
