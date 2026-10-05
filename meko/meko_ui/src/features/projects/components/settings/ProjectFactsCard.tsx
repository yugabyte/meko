// What this project is: name, the layout it was stamped from, when, and who
// can reach it. Read-only; every control lives in a sibling card.

import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { formatDatapackCreatedDateShorthand } from "@/lib/datapackDateFormat";
import type { Project, ProjectMembers } from "@/lib/api/projectsApi";
import { useProjectThemes } from "@/features/projects/hooks/useProjects";
import { countLabel } from "@/features/projects/projectsCopy";
import { LABEL_NAME_FONT } from "@/features/projects/projectsTypography";

/** One fact. `mono` marks values that are identifiers rather than prose; the
 *  typeface it resolves to is projectsTypography's to decide, and today
 *  both are sans. */
function FactRow({
  label,
  value,
  mono = false,
}: {
  label: string;
  value: string;
  mono?: boolean;
}) {
  return (
    <div className="flex items-baseline justify-between gap-4 py-2.5">
      <dt className="shrink-0 text-sm text-muted-foreground">{label}</dt>
      <dd
        className={cn("min-w-0 truncate text-right text-sm text-foreground", mono && LABEL_NAME_FONT)}
      >
        {value}
      </dd>
    </div>
  );
}

export function ProjectFactsCard({
  project,
  members,
}: {
  project: Project;
  members: ProjectMembers;
}) {
  // Cached for the session, so this costs no request here.
  const { data: themes = [] } = useProjectThemes();
  // The layout's LABEL, not its key, and no version: theme_version is
  // hardcoded "1.0" for every theme and nothing reads it back, so showing it
  // told the user a constant. Falls back to the stored key if the layout has
  // since been removed from the registry.
  const layoutLabel =
    themes.find((t) => t.key === project.theme_key)?.label ?? project.theme_key;

  return (
    <Card className="border-border bg-card p-6">
      <h2 className="text-base font-semibold text-foreground">Project</h2>
      <dl className="mt-3 divide-y divide-border">
        <FactRow label="Name" value={project.project_name} mono />
        <FactRow label="Created from" value={layoutLabel} />
        <FactRow label="Created" value={formatDatapackCreatedDateShorthand(project.created_at)} />
        <FactRow label="Owner" value={project.owner_name || "You"} />
        <FactRow label="Members" value={countLabel(members.people, "person", "people")} />
        <FactRow label="Agents active" value={countLabel(members.agents, "agent")} />
      </dl>
    </Card>
  );
}
