// Creates a project from a layout (whose folders it starts with, or one folder the user names for a
// custom one), or edits a project's name and description when given it. Names are 2 to 60
// characters, with no /.

import { useState } from "react";
import { api } from "../lib/api";
import type { Project } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Field, Input } from "./ui";

export const NAME_MIN = 2;
export const NAME_MAX = 60;

// Whether a project's or folder's name would be taken: the server says why when it isn't.
export const nameOK = (name: string) => name.trim().length >= NAME_MIN && !name.includes("/");

export function ProjectDialog({
  project,
  onClose,
  onSaved,
}: {
  project?: Project;
  onClose: () => void;
  onSaved: (id: string) => void;
}) {
  const themes = useLoad(() => (project ? Promise.resolve([]) : api.projectThemes()), [project]);
  const [name, setName] = useState(project?.project_name ?? "");
  const [description, setDescription] = useState(project?.description ?? "");
  const [theme, setTheme] = useState("");
  const [folder, setFolder] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const picked = themes.data?.find((t) => t.key === theme) ?? themes.data?.[0];
  const needsFolder = !project && picked !== undefined && picked.folders.length === 0;

  const submit = async () => {
    setBusy(true);
    setError(undefined);
    try {
      if (project) {
        await api.updateProject(project.project_id, { project_name: name.trim(), description: description.trim() });
        onSaved(project.project_id);
      } else {
        const made = await api.createProject({
          project_name: name.trim(),
          description: description.trim(),
          theme_key: picked?.key ?? "custom",
          ...(needsFolder ? { folders: [folder.trim()] } : {}),
        });
        onSaved(made.project_id);
      }
    } catch (e) {
      setError(e as Error);
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={project ? "Edit project" : "New project"}
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={busy || !nameOK(name) || (needsFolder && !nameOK(folder)) || (!project && !picked)}
            onClick={submit}
          >
            {project ? "Save" : "Create"}
          </Button>
        </>
      }
    >
      <Field label="Name">
        <Input autoFocus value={name} maxLength={NAME_MAX} onChange={(e) => setName(e.target.value)} />
      </Field>
      <Field label="Description">
        <Input
          value={description}
          onChange={(e) => setDescription(e.target.value)}
          placeholder="What outcome do these datapacks serve?"
        />
      </Field>
      {!project && (
        <Field label="Layout" hint={picked?.description}>
          <select
            value={picked?.key ?? ""}
            onChange={(e) => setTheme(e.target.value)}
            className="h-9 w-full rounded-lg border bg-card px-3 text-sm"
          >
            {themes.data?.map((t) => (
              <option key={t.key} value={t.key}>
                {t.label}
              </option>
            ))}
          </select>
        </Field>
      )}
      {picked && picked.folders.length > 0 && !project && (
        <p className="text-xs text-muted-foreground">
          Starts with the folders{" "}
          {picked.folders.map((f, i) => (
            <span key={f}>
              {i > 0 && ", "}
              <code>{f}</code>
            </span>
          ))}
          . You can add, rename and delete folders later.
        </p>
      )}
      {needsFolder && (
        <Field label="First folder">
          <Input value={folder} maxLength={NAME_MAX} onChange={(e) => setFolder(e.target.value)} />
        </Field>
      )}
      {themes.error && <ErrorNote error={themes.error} />}
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
