// One project on the Projects list: name, description, the counts that matter,
// and the shape of its folder tree. Clicking opens the project.

import { Database, Folder, Sparkles } from "lucide-react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { formatDatapackCreatedDateShorthand } from "@/lib/datapackDateFormat";
import type { Project } from "@/lib/api/projectsApi";
import { ProjectFolderChips } from "@/features/projects/components/ProjectFolderChips";
import { LABEL_NAME_FONT } from "@/features/projects/projectsTypography";

/** One count on a project card. Two of these sit side by side. */
function ProjectStat({
  icon: Icon,
  label,
  value,
}: {
  icon: typeof Database;
  label: string;
  value: number;
}) {
  return (
    <div className="flex-1 rounded-lg border border-border bg-muted/30 px-3 py-2">
      <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
        <Icon className="h-3.5 w-3.5 shrink-0" aria-hidden />
        {label}
      </div>
      <div className="mt-0.5 text-2xl font-semibold tabular-nums text-foreground">{value}</div>
    </div>
  );
}

export function ProjectCard({
  project,
  learningsCount,
  onOpen,
}: {
  project: Project;
  /** Summed across the project's datapacks. Reads 0 until the datapack list
   *  endpoint carries per-datapack counts (MEKO-653). */
  learningsCount: number;
  onOpen: (project: Project) => void;
}) {
  const description = (project.description ?? "").trim();

  return (
    <Card
      className={cn(
        "group flex h-full flex-col gap-0 overflow-hidden border-border bg-card p-0 shadow-sm transition-colors",
        "hover:border-brand/40 hover:bg-card/90",
      )}
    >
      <div
        role="button"
        tabIndex={0}
        onClick={() => onOpen(project)}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onOpen(project);
          }
        }}
        aria-label={`Open project ${project.project_name}`}
        className="flex flex-1 cursor-pointer flex-col gap-4 p-5 text-left focus:outline-none focus-visible:ring-2 focus-visible:ring-brand/50"
      >
        <div className="flex items-start gap-3">
          <div
            className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-brand/15 ring-1 ring-brand/20"
            aria-hidden
          >
            <Folder className="h-4 w-4 text-brand" />
          </div>
          <div className="min-w-0 flex-1">
            <h3
              className={cn("truncate text-sm font-semibold text-foreground", LABEL_NAME_FONT)}
              title={project.project_name}
            >
              {project.project_name}
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Created {formatDatapackCreatedDateShorthand(project.created_at)}
            </p>
          </div>
        </div>

        {description ? (
          <p className="line-clamp-2 text-sm leading-relaxed text-muted-foreground">
            {description}
          </p>
        ) : null}

        <div className="flex gap-3">
          <ProjectStat icon={Database} label="Datapacks" value={project.datapack_count} />
          <ProjectStat icon={Sparkles} label="Learnings" value={learningsCount} />
        </div>
      </div>

      <div className="border-t border-border bg-muted/20 px-5 py-3">
        <ProjectFolderChips folders={project.folder_names ?? []} />
      </div>
    </Card>
  );
}
