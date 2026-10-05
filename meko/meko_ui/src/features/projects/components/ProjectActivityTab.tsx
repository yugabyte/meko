// The project's Activity tab: what has been promoted, and the feed of
// uploads and promotions across its datapacks.
//
// One tile, not four. "Changes · 30d", "By agents" and "By people" all count
// events, and nothing records events — the feed below is derived from content
// that still exists. Promotions is the one number we already hold, from the
// same rollup the tree and the panel use, so it costs nothing.

import { useCallback } from "react";
import { Sparkles } from "lucide-react";
import { StatTile } from "@/components/ui/stat-tile";
import { KnowledgeActivityLog } from "@/features/datapacks/components/knowledge-documents/KnowledgeActivityLog";
import { listProjectActivity } from "@/lib/api/projectsApi";
import type { ProjectTotals } from "@/features/projects/lib/projectTree";
import {
  ACTIVITY_TAB_DESCRIPTION,
  countLabel,
  PROMOTIONS_TILE_TITLE,
} from "@/features/projects/projectsCopy";

export function ProjectActivityTab({
  projectId,
  totals,
}: {
  projectId: string;
  totals: ProjectTotals;
}) {
  // Stable identity: the feed refetches from page one whenever this changes.
  const fetchPage = useCallback(
    (args: { limit: number; cursor: string }) => listProjectActivity(projectId, args),
    [projectId],
  );

  return (
    <div className="space-y-6">
      <div className="max-w-xs">
        <StatTile
          icon={<Sparkles className="h-3.5 w-3.5 text-brand" aria-hidden />}
          label={PROMOTIONS_TILE_TITLE}
          value={totals.promoted}
          hint={`${countLabel(totals.awaitingReview, "memory", "memories")} awaiting review`}
        />
      </div>

      <KnowledgeActivityLog
        fetchPage={fetchPage}
        description={ACTIVITY_TAB_DESCRIPTION}
        pageSize={10}
      />
    </div>
  );
}
