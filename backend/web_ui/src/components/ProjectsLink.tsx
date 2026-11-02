// The sidebar's link to Projects: the datapacks filed in projects and their folders, as Meko Cloud's.

import { FolderKanban } from "lucide-react";
import { href, navigate, PROJECTS } from "../lib/route";
import { cx } from "./ui";

export function ProjectsLink({ active }: { active: boolean }) {
  return (
    <a
      href={href(PROJECTS)}
      onClick={(e) => (e.preventDefault(), navigate(PROJECTS))}
      aria-current={active ? "page" : undefined}
      className={cx(
        "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm",
        active ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
      )}
    >
      <FolderKanban className={cx("size-4 shrink-0", active && "text-primary")} />
      Projects
    </a>
  );
}
