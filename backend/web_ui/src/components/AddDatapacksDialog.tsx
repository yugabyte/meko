// Files datapacks in a project's folder: this Meko's own, picked from a list that says where each
// one is filed now (filing one moves it). Each is filed on its own, so one that fails leaves the rest
// filed and says which.

import { useState } from "react";
import { api } from "../lib/api";
import { matchesName, rowLink } from "../lib/cloud";
import type { Datapack, ProjectFolder } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Input, Spinner } from "./ui";

export function AddDatapacksDialog({
  folder,
  onClose,
  onFiled,
}: {
  folder: ProjectFolder;
  onClose: () => void;
  onFiled: () => void;
}) {
  const loaded = useLoad(async () => {
    const [all, placed] = await Promise.all([api.datapacks(), api.placements()]);
    // Only this Meko's: Meko Cloud's open in its web app, and are filed in its projects.
    return { mine: all.filter((d) => rowLink(d).kind === "here"), placed };
  }, []);
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();

  const shown = (loaded.data?.mine ?? []).filter(
    (d: Datapack) => matchesName(d.datapack_name, query) && loaded.data?.placed[d.datapack_id]?.folder_id !== folder.folder_id,
  );
  const toggle = (id: string) =>
    setPicked((p) => {
      const n = new Set(p);
      if (!n.delete(id)) n.add(id);
      return n;
    });

  const file = async () => {
    setBusy(true);
    setError(undefined);
    const results = await Promise.allSettled([...picked].map((id) => api.placeDatapack(id, folder.folder_id)));
    const failed = results.filter((r): r is PromiseRejectedResult => r.status === "rejected");
    if (failed.length === 0) return onFiled();
    setError(new Error(`${failed.length} of ${results.length} couldn't be filed: ${(failed[0].reason as Error).message}`));
    setBusy(false);
  };

  return (
    <Dialog
      title={`Add datapacks to ${folder.folder_name}`}
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={busy || picked.size === 0} onClick={file}>
            Add {picked.size || ""}
          </Button>
        </>
      }
    >
      <Input
        autoFocus
        type="search"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
        placeholder="Find a datapack…"
        aria-label="Find a datapack"
      />
      {loaded.error && <ErrorNote error={loaded.error} />}
      {!loaded.data && !loaded.error && <Spinner />}
      {loaded.data && shown.length === 0 && (
        <p className="text-sm text-muted-foreground">No other datapacks on this computer{query.trim() && " match"}.</p>
      )}
      <ul className="space-y-1">
        {shown.map((d) => {
          const now = loaded.data?.placed[d.datapack_id];
          return (
            <li key={d.datapack_id}>
              <label className="flex cursor-pointer items-start gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-accent">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={picked.has(d.datapack_id)}
                  onChange={() => toggle(d.datapack_id)}
                />
                <span className="min-w-0 flex-1">
                  <span className="block truncate">{d.datapack_name}</span>
                  {now && <span className="block text-xs text-muted-foreground">In {now.path}: will move</span>}
                </span>
              </label>
            </li>
          );
        })}
      </ul>
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}
