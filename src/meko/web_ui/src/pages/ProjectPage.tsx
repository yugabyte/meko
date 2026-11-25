// One project: its folders, each with the datapacks filed in it, and its activity and totals.
// Folders can be added, renamed, described and deleted, and datapacks added to a folder (moved, if
// filed elsewhere) or taken out. Deleting a folder or the project unfiles its datapacks; it never
// deletes them.

import { useEffect, useState } from "react";
import { ChevronRight, Folder, FolderPlus, Pencil, Plus, Trash2 } from "lucide-react";
import { AddDatapacksDialog } from "../components/AddDatapacksDialog";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { FolderDialog } from "../components/FolderDialog";
import { KnowledgeActivityLog } from "../components/KnowledgeActivityLog";
import { ProjectDialog } from "../components/ProjectDialog";
import { Segmented } from "../components/Segmented";
import { Button, Card, cx, ErrorNote, Loading } from "../components/ui";
import { api } from "../lib/api";
import { bytes, count, dateTime, timeAgo } from "../lib/format";
import { href, navigate, PROJECTS } from "../lib/route";
import type { FiledDatapack, ProjectDetail, ProjectFolder } from "../lib/types";
import { useLoad } from "../lib/useLoad";

const TABS = [
  { id: "folders", label: "Folders" },
  { id: "activity", label: "Activity" },
  { id: "settings", label: "Settings" },
] as const;
type ProjectTab = (typeof TABS)[number]["id"];

export function ProjectPage({ id }: { id: string }) {
  const detail = useLoad(() => api.project(id), [id]);
  const [tab, setTab] = useState<ProjectTab>("folders");
  const [editing, setEditing] = useState(false);
  // Each change to the tree, so the activity reloads with it.
  const [changes, setChanges] = useState(0);
  const changed = () => {
    detail.reload();
    setChanges((n) => n + 1);
  };

  if (detail.error) return <ErrorNote error={detail.error} />;
  if (!detail.data) return <Loading />;
  const { project, folders, datapacks } = detail.data;
  return (
    <div className="space-y-4">
      <nav className="flex items-center gap-1 text-sm text-muted-foreground">
        <a href={href(PROJECTS)} onClick={(e) => (e.preventDefault(), navigate(PROJECTS))} className="hover:underline">
          Projects
        </a>
        <ChevronRight className="size-3.5" />
        <span className="truncate text-foreground">{project.project_name}</span>
      </nav>
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <h2 className="text-lg font-semibold">{project.project_name}</h2>
          <p className="text-sm text-muted-foreground">
            {count(datapacks.length)} datapacks in {count(folders.length)} folders · made {dateTime(project.created_at)}
          </p>
          {project.description && <p className="text-sm">{project.description}</p>}
        </div>
        <Button size="sm" variant="outline" onClick={() => setEditing(true)}>
          <Pencil className="size-3.5" /> Edit
        </Button>
      </header>
      <Segmented options={TABS} value={tab} onChange={setTab} />
      {tab === "folders" ? (
        <FoldersTab detail={detail.data} onChanged={changed} />
      ) : tab === "activity" ? (
        <KnowledgeActivityLog projectId={id} refresh={changes} />
      ) : (
        <SettingsTab detail={detail.data} />
      )}
      {editing && (
        <ProjectDialog
          project={project}
          onClose={() => setEditing(false)}
          onSaved={() => {
            setEditing(false);
            changed();
          }}
        />
      )}
    </div>
  );
}

function FoldersTab({ detail, onChanged }: { detail: ProjectDetail; onChanged: () => void }) {
  const { project, folders, datapacks } = detail;
  const [openID, setOpenID] = useState(folders[0]?.folder_id);
  // A folder just made, which opens once the reloaded project lists it.
  const [made, setMade] = useState<string>();
  const [dialog, setDialog] = useState<"add-folder" | "edit-folder" | "delete-folder" | "add-datapacks">();
  useEffect(() => {
    if (made && folders.some((f) => f.folder_id === made)) {
      setOpenID(made);
      setMade(undefined);
    } else if (!made && !folders.some((f) => f.folder_id === openID)) {
      // The open folder was deleted.
      setOpenID(folders[0]?.folder_id);
    }
  }, [folders, openID, made]);
  const open = folders.find((f) => f.folder_id === openID);
  const inOpen = datapacks.filter((d) => d.folder_id === openID);
  const done = () => {
    setDialog(undefined);
    onChanged();
  };

  return (
    <div className="grid gap-4 md:grid-cols-[14rem_1fr]">
      <Card className="space-y-0.5 self-start p-2">
        {folders.map((f) => (
          <button
            key={f.folder_id}
            type="button"
            onClick={() => setOpenID(f.folder_id)}
            aria-current={f.folder_id === openID ? "true" : undefined}
            className={cx(
              "flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-sm",
              f.folder_id === openID ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
            )}
          >
            <Folder className={cx("size-4 shrink-0", f.folder_id === openID && "text-primary")} />
            <span className="min-w-0 flex-1 truncate">{f.folder_name}</span>
            <span className="text-xs">{f.datapack_count}</span>
          </button>
        ))}
        <Button size="sm" variant="ghost" className="w-full justify-start" onClick={() => setDialog("add-folder")}>
          <FolderPlus className="size-3.5" /> Add folder
        </Button>
      </Card>

      {open ? (
        <section className="min-w-0 space-y-3">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div className="min-w-0">
              <h3 className="font-medium">{open.folder_name}</h3>
              {open.folder_description && <p className="text-sm text-muted-foreground">{open.folder_description}</p>}
            </div>
            <div className="flex items-center gap-1">
              <Button size="sm" variant="primary" onClick={() => setDialog("add-datapacks")}>
                <Plus className="size-3.5" /> Add datapacks
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setDialog("edit-folder")} title="Edit the folder">
                <Pencil className="size-3.5" />
              </Button>
              <Button size="sm" variant="danger" onClick={() => setDialog("delete-folder")} title="Delete the folder">
                <Trash2 className="size-3.5" />
              </Button>
            </div>
          </div>
          {inOpen.length === 0 ? (
            <p className="text-sm text-muted-foreground">No datapacks in this folder yet.</p>
          ) : (
            <Card className="divide-y">
              {inOpen.map((d) => (
                <DatapackRow key={d.datapack_id} d={d} onRemoved={onChanged} />
              ))}
            </Card>
          )}
        </section>
      ) : (
        <p className="text-sm text-muted-foreground">No folders: add one to file datapacks in.</p>
      )}

      {dialog === "add-folder" && (
        <FolderDialog
          projectId={project.project_id}
          onClose={() => setDialog(undefined)}
          onSaved={(fid) => {
            setMade(fid);
            done();
          }}
        />
      )}
      {dialog === "edit-folder" && open && (
        <FolderDialog projectId={project.project_id} folder={open} onClose={() => setDialog(undefined)} onSaved={done} />
      )}
      {dialog === "delete-folder" && open && (
        <ConfirmDialog
          title="Delete folder"
          action="Delete folder"
          onClose={() => setDialog(undefined)}
          onConfirm={async () => {
            await api.deleteFolder(project.project_id, open.folder_id);
            done();
          }}
        >
          <FolderDeleteNote folder={open} />
        </ConfirmDialog>
      )}
      {dialog === "add-datapacks" && open && (
        <AddDatapacksDialog folder={open} onClose={() => setDialog(undefined)} onFiled={done} />
      )}
    </div>
  );
}

function FolderDeleteNote({ folder }: { folder: ProjectFolder }) {
  return (
    <>
      This deletes the folder <b>{folder.folder_name}</b>.
      {folder.datapack_count > 0 &&
        ` Its ${folder.datapack_count} datapacks aren't deleted: they leave the project and keep working.`}
    </>
  );
}

function DatapackRow({ d, onRemoved }: { d: FiledDatapack; onRemoved: () => void }) {
  const [error, setError] = useState<Error>();
  const to = { datapackId: d.datapack_id, tab: "overview" as const };
  const remove = async () => {
    try {
      await api.unplaceDatapack(d.datapack_id);
      onRemoved();
    } catch (e) {
      setError(e as Error);
    }
  };
  return (
    <div className="space-y-1 px-3 py-2">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <a
            href={href(to)}
            onClick={(e) => (e.preventDefault(), navigate(to))}
            className="block truncate text-sm font-medium hover:underline"
          >
            {d.datapack_name}
          </a>
          <p className="text-xs text-muted-foreground">
            {count(d.memory_count)} memories · {count(d.awaiting_review_count)} learnings · {count(d.promoted_count)}{" "}
            promoted · {count(d.knowledge_count)} files · filed {timeAgo(d.placed_at)}
          </p>
        </div>
        <Button size="sm" variant="ghost" onClick={remove} title="Take it out of the project; it isn't deleted">
          Remove
        </Button>
      </div>
      {error && <ErrorNote error={error} />}
    </div>
  );
}

function SettingsTab({ detail }: { detail: ProjectDetail }) {
  const { project } = detail;
  const stats = useLoad(() => api.projectStats(project.project_id), [project.project_id]);
  const [deleting, setDeleting] = useState(false);
  const s = stats.data;
  return (
    <div className="space-y-4">
      <Card className="space-y-2 p-4">
        <h3 className="font-medium">What it holds</h3>
        {stats.error && <ErrorNote error={stats.error} />}
        {s && (
          <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm sm:grid-cols-3">
            {[
              ["Searches", count(s.searches_hybrid + s.searches_kb + s.searches_memory)],
              ["Conversations", count(s.conversations)],
              ["Knowledge files", count(s.kb_files)],
              ["Knowledge chunks", count(s.kb_chunks)],
              ["Artifacts", count(s.artifacts)],
              ["Artifact size", bytes(s.artifact_bytes)],
            ].map(([k, v]) => (
              <div key={k}>
                <dt className="text-xs text-muted-foreground">{k}</dt>
                <dd className="font-medium">{v}</dd>
              </div>
            ))}
          </dl>
        )}
        <p className="text-xs text-muted-foreground">Searches count the last 90 days.</p>
      </Card>
      <Card className="space-y-2 border-destructive/40 p-4">
        <h3 className="font-medium">Delete project</h3>
        <p className="text-sm text-muted-foreground">
          Its folders go. Its datapacks aren't deleted: they leave the project and keep working.
        </p>
        <Button size="sm" variant="danger" onClick={() => setDeleting(true)}>
          <Trash2 className="size-3.5" /> Delete project
        </Button>
      </Card>
      {deleting && (
        <ConfirmDialog
          title="Delete project"
          name={project.project_name}
          action="Delete project"
          onClose={() => setDeleting(false)}
          onConfirm={async () => {
            await api.deleteProject(project.project_id);
            navigate(PROJECTS);
          }}
        >
          This deletes <b>{project.project_name}</b> and its folders. Its {detail.datapacks.length} datapacks aren't
          deleted. Type its name to confirm.
        </ConfirmDialog>
      )}
    </div>
  );
}
