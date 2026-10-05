// Settings for one project: what it is, and the two deletes that cannot be
// undone. Layout only; each card owns its own state and dialogs.
//
// No folder operations here. Add and Rename live in the page header beside
// the tree they act on, and moving a datapack is reachable from the Datapacks
// grid and from the datapack's own page. A third copy was just a second place
// to keep in step.
//
// Everything here is owner-only, which the caller enforces by not rendering
// the tab at all for anyone else. Nothing below re-checks it.

import type { Datapack } from "@/lib/api/mekoApi";
import type { ProjectDetail } from "@/lib/api/projectsApi";
import { ProjectFactsCard } from "@/features/projects/components/settings/ProjectFactsCard";
import { ProjectDangerZoneCard } from "@/features/projects/components/settings/ProjectDangerZoneCard";

export function ProjectSettingsTab({
  detail,
  ownDatapacks,
  onProjectDeleted,
}: {
  detail: ProjectDetail;
  /** Every datapack the signed-in user owns, for the move and delete pickers. */
  ownDatapacks: Datapack[];
  onProjectDeleted: () => void;
}) {
  const { project, datapacks, members } = detail;

  return (
    <div className="grid gap-6 lg:grid-cols-2">
      <ProjectFactsCard project={project} members={members} />

      <div className="space-y-6">
        <ProjectDangerZoneCard
          project={project}
          datapacks={datapacks}
          ownDatapacks={ownDatapacks}
          onProjectDeleted={onProjectDeleted}
        />
      </div>
    </div>
  );
}
