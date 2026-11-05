// Moves this datapack to the linked Meko Cloud: first whether it fits the plan, each limit shown,
// then the sync, all or nothing. Once it's done, the datapack is gone from this computer and lives
// in Meko Cloud, where every call on it goes.

import { useEffect, useState } from "react";
import { Check, Cloud, ExternalLink, X } from "lucide-react";
import { api } from "../lib/api";
import { bytes, count } from "../lib/format";
import type { Datapack, SyncCheck, SyncRun } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Spinner } from "./ui";

const POLL_MS = 2000;

const CHECKS: Record<string, { label: string; size?: boolean }> = {
  datapacks: { label: "Datapacks" },
  memory_adds: { label: "Memories added" },
  kb_storage: { label: "Knowledge file storage", size: true },
  kb_file_size: { label: "Largest file", size: true },
  searchable_turns: { label: "Searchable conversation turns" },
};

const STEPS: Record<string, string> = {
  exporting: "Packing it up…",
  uploading: "Uploading it to Meko Cloud…",
  importing: "Meko Cloud is importing it…",
};

export function CloudSyncDialog({
  datapack,
  onClose,
  onSynced,
}: {
  datapack: Datapack;
  onClose: () => void;
  onSynced: () => void;
}) {
  const id = datapack.datapack_id;
  const precheck = useLoad(() => api.syncPrecheck(id), [id]);
  const [run, setRun] = useState<SyncRun>();
  const [error, setError] = useState<Error>();

  useEffect(() => {
    if (run?.state !== "syncing") return;
    const t = setTimeout(() => {
      api.syncStatus(id).then(setRun, (e: Error) => setError(e));
    }, POLL_MS);
    return () => clearTimeout(t);
  }, [id, run]);

  const start = async () => {
    setError(undefined);
    try {
      setRun(await api.startSync(id));
    } catch (e) {
      setError(e as Error);
    }
  };

  const done = run?.state === "done";
  const checks = run?.precheck?.checks ?? precheck.data?.checks ?? [];
  const fits = (run?.precheck ?? precheck.data)?.ok ?? false;
  const name = datapack.datapack_name;

  return (
    <Dialog
      title={done ? `${name} is in Meko Cloud` : `Sync ${name} to Meko Cloud`}
      onClose={done ? onSynced : onClose}
      actions={
        done ? (
          <>
            {run.web_url && (
              <a
                href={run.web_url}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex h-9 items-center gap-1.5 rounded-lg border bg-card px-3 text-sm font-medium hover:bg-accent"
              >
                <ExternalLink className="size-3.5" /> Open in Meko Cloud
              </a>
            )}
            <Button variant="primary" onClick={onSynced}>
              Done
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose} disabled={run?.state === "syncing"}>
              Cancel
            </Button>
            <Button variant="primary" disabled={!fits || !!run} onClick={start}>
              <Cloud className="size-4" /> Sync to Meko Cloud
            </Button>
          </>
        )
      }
    >
      {done ? (
        <p className="text-sm">
          <b>{name}</b> lives in Meko Cloud now, and it's gone from this computer. Agents keep using it as before: every
          call on it goes to Meko Cloud.
        </p>
      ) : (
        <>
          <p className="text-sm text-muted-foreground">
            Everything in <b>{name}</b> moves to Meko Cloud in one go. Once it's there, it's deleted from this computer,
            and agents reach it in Meko Cloud with no change on their side. If anything goes wrong, it stays here as it
            is.
          </p>
          {precheck.error && <ErrorNote error={precheck.error} />}
          {!precheck.data && !precheck.error && (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Spinner className="size-3.5" /> Checking it fits your Meko Cloud plan…
            </p>
          )}
          {checks.length > 0 && (
            <ul className="space-y-1.5 text-sm">
              {checks.map((c) => (
                <CheckRow key={c.check} c={c} />
              ))}
            </ul>
          )}
          {precheck.data && !fits && (
            <p className="text-sm">
              It doesn't fit your Meko Cloud <b>{precheck.data.plan}</b> plan. Upgrade your plan, or delete datapacks in
              Meko Cloud, to sync it.
            </p>
          )}
          {run?.state === "syncing" && (
            <p className="flex items-center gap-2 text-sm">
              <Spinner className="size-3.5" /> {STEPS[run.step] ?? "Syncing…"} Writes to it are refused until it's done.
            </p>
          )}
          {run?.state === "failed" && !run.precheck && (
            <ErrorNote error={new Error(`It wasn't synced, and it's still here as it was: ${run.error}`)} />
          )}
        </>
      )}
      {error && <ErrorNote error={error} />}
    </Dialog>
  );
}

function CheckRow({ c }: { c: SyncCheck }) {
  const known = CHECKS[c.check] ?? { label: c.check };
  const show = (n: number) => (known.size ? bytes(n) : count(n));
  const total = c.check === "kb_file_size" ? c.adding : c.used + c.adding;
  return (
    <li className="flex items-start gap-2">
      {c.ok ? (
        <Check className="mt-0.5 size-4 shrink-0 text-success" aria-label="fits" />
      ) : (
        <X className="mt-0.5 size-4 shrink-0 text-destructive" aria-label="over the limit" />
      )}
      <span className="min-w-0 flex-1">
        <span className="font-medium">{known.label}</span>{" "}
        <span className="text-muted-foreground">
          {show(total)} of {c.limit < 0 ? "no limit" : show(c.limit)}
        </span>
        {!c.ok && c.message && <span className="block text-xs text-destructive">{c.message}</span>}
      </span>
    </li>
  );
}
