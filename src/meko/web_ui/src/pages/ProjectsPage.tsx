// Projects: each groups datapacks that work toward one outcome, in folders, as Meko Cloud's do. A
// card per project, newest first, with a box that finds one by its name or a folder's.

import { useState } from "react";
import { FolderKanban, Plus, Search } from "lucide-react";
import { ProjectDialog } from "../components/ProjectDialog";
import { Button, Card, Empty, ErrorNote, Input, Loading } from "../components/ui";
import { api } from "../lib/api";
import { matchesName } from "../lib/cloud";
import { count, dateTime } from "../lib/format";
import { href, navigate, project } from "../lib/route";
import type { Project } from "../lib/types";
import { useLoad } from "../lib/useLoad";

export function ProjectsPage() {
  const list = useLoad(() => api.projects(), []);
  const [creating, setCreating] = useState(false);
  const [query, setQuery] = useState("");
  const shown = (list.data ?? []).filter(
    (p) => matchesName(p.project_name, query) || p.folder_names.some((f) => matchesName(f, query)),
  );
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Projects</h2>
          <p className="text-sm text-muted-foreground">
            A project groups the datapacks that work toward one outcome, in folders: one per kind or stage of work.
          </p>
        </div>
        <Button variant="primary" onClick={() => setCreating(true)}>
          <Plus className="size-4" /> New project
        </Button>
      </div>
      {list.error && <ErrorNote error={list.error} />}
      {!list.data && !list.error ? (
        <Loading />
      ) : list.data?.length === 0 ? (
        <Empty icon={<FolderKanban className="size-8" />} title="No projects yet">
          Make one with New project, then add folders and file datapacks in them.
        </Empty>
      ) : (
        <>
          <div className="relative max-w-sm">
            <Search className="pointer-events-none absolute left-2.5 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
            <Input
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Find a project or folder…"
              aria-label="Find a project or folder"
              className="pl-8"
            />
          </div>
          {shown.length === 0 && <p className="text-sm text-muted-foreground">No projects match “{query.trim()}”.</p>}
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {shown.map((p) => (
              <ProjectCard key={p.project_id} p={p} />
            ))}
          </div>
        </>
      )}
      {creating && (
        <ProjectDialog
          onClose={() => setCreating(false)}
          onSaved={(id) => {
            setCreating(false);
            navigate(project(id));
          }}
        />
      )}
    </div>
  );
}

function ProjectCard({ p }: { p: Project }) {
  const to = project(p.project_id);
  return (
    <a href={href(to)} onClick={(e) => (e.preventDefault(), navigate(to))} className="block">
      <Card className="h-full space-y-2 p-4 transition hover:border-primary">
        <div className="flex items-start justify-between gap-2">
          <h3 className="min-w-0 truncate font-medium">{p.project_name}</h3>
          <span className="shrink-0 text-xs text-muted-foreground">{dateTime(p.created_at)}</span>
        </div>
        {p.description && <p className="line-clamp-2 text-sm text-muted-foreground">{p.description}</p>}
        <p className="text-sm">
          <b>{count(p.datapack_count)}</b> <span className="text-muted-foreground">datapacks in</span>{" "}
          <b>{count(p.folder_count)}</b> <span className="text-muted-foreground">folders</span>
        </p>
        <div className="flex flex-wrap gap-1">
          {p.folder_names.map((f) => (
            <span key={f} className="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
              {f}
            </span>
          ))}
        </div>
      </Card>
    </a>
  );
}
