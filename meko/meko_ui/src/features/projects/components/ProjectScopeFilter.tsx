// Project filter for the Datapacks list: all projects, one project, or the
// unfiled set. Sits beside the owned/shared tabs rather than replacing them —
// the two narrow on different axes (who can see it vs. where it lives).

import { Folder } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Project } from "@/lib/api/projectsApi";
import {
  PROJECT_FILTER_ALL,
  PROJECT_FILTER_UNFILED,
} from "@/features/projects/lib/filterDatapacksByProject";
import { UNFILED_LABEL } from "@/features/projects/projectsCopy";

export function ProjectScopeFilter({
  projects,
  value,
  onChange,
}: {
  projects: Project[];
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger
        className="h-9 w-full sm:w-auto sm:min-w-[170px]"
        aria-label="Filter datapacks by project"
      >
        <Folder className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={PROJECT_FILTER_ALL}>All projects</SelectItem>
        <SelectItem value={PROJECT_FILTER_UNFILED}>{UNFILED_LABEL}</SelectItem>
        {projects.length > 0 ? <SelectSeparator /> : null}
        {projects.map((project) => (
          <SelectItem key={project.project_id} value={project.project_id}>
            {project.project_name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
