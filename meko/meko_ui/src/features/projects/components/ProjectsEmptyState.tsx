// Shown when the workspace has no projects yet. Built on the shared Empty
// primitive so it matches every other zero state in the app.

import { FolderPlus } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  NEW_PROJECT_LABEL,
  PROJECTS_EMPTY_BODY,
  PROJECTS_EMPTY_TITLE,
} from "@/features/projects/projectsCopy";

export function ProjectsEmptyState({ onCreate }: { onCreate: () => void }) {
  return (
    <Empty className="rounded-xl border border-dashed border-border bg-card/40">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <FolderPlus className="h-6 w-6" aria-hidden />
        </EmptyMedia>
        <EmptyTitle>{PROJECTS_EMPTY_TITLE}</EmptyTitle>
        <EmptyDescription>{PROJECTS_EMPTY_BODY}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button type="button" onClick={onCreate} className="gap-2">
          <FolderPlus className="h-4 w-4" />
          {NEW_PROJECT_LABEL}
        </Button>
      </EmptyContent>
    </Empty>
  );
}
