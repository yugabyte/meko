// Moves this datapack to the linked Meko Cloud: first whether it fits the plan, each limit shown,
// then the sync, all or nothing, on a live timeline of its steps. Once it's done, the datapack is gone
// from this computer and lives in Meko Cloud. With no Meko Cloud link yet, it asks for the API key.
// Because the sync deletes it here, it starts only once the person ticks that they understand and
// types the datapack's name, as deleting a datapack or a project asks.

import { useEffect, useState } from "react";
import { Check, Cloud, ExternalLink, KeyRound, X } from "lucide-react";
import { ApiError, api } from "../lib/api";
import { bytes, clock, count } from "../lib/format";
import { syncElapsed } from "../lib/syncTimeline";
import { MEKO_CLOUD, navigate } from "../lib/route";
import type { Datapack, SyncCheck, SyncRun } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { Dialog } from "./Dialog";
import { SyncTimeline } from "./SyncTimeline";
import { Button, ErrorNote, Input, Spinner } from "./ui";

const POLL_MS = 1000;

const CHECKS: Record<string, { label: string; size?: boolean }> = {
  datapacks: { label: "Datapacks" },
  memory_adds: { label: "Memories added" },
  kb_storage: { label: "Knowledge file storage", size: true },
  kb_file_size: { label: "Largest file", size: true },
  searchable_turns: { label: "Searchable conversation turns" },
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
  const [now, setNow] = useState(() => Date.now());
  const [agreed, setAgreed] = useState(false);
  const [typed, setTyped] = useState("");
  const syncing = run?.state === "syncing";

  // Opened again while it syncs (closing doesn't stop it): pick the sync up where it is.
  useEffect(() => {
    api.syncStatus(id).then(
      (r) => r.state === "syncing" && setRun((cur) => cur ?? r),
      () => {}, // none since Meko started
    );
  }, [id]);

  useEffect(() => {
    if (!syncing) return;
    const t = setTimeout(() => {
      api.syncStatus(id).then(setRun, (e: Error) => setError(e));
    }, POLL_MS);
    return () => clearTimeout(t);
  }, [id, run, syncing]);

  // The clocks tick between polls.
  useEffect(() => {
    setNow(Date.now());
    if (!syncing) return;
    const t = setInterval(() => setNow(Date.now()), 250);
    return () => clearInterval(t);
  }, [syncing]);

  const start = async () => {
    setError(undefined);
    try {
      setRun(await api.startSync(id));
    } catch (e) {
      setError(e as Error);
    }
  };

  const done = run?.state === "done";
  // A run that got past the precheck has a timeline to show.
  const timeline = run && !run.precheck ? run : undefined;
  const checks = run?.precheck?.checks ?? precheck.data?.checks ?? [];
  const fits = (run?.precheck ?? precheck.data)?.ok ?? false;
  const name = datapack.datapack_name;
  // Asked once it fits and before it starts; the sync button waits for both answers.
  const confirming = fits && !run && !!precheck.data;
  const confirmed = agreed && typed === name;
  // Meko Cloud isn't linked: the API key comes first, in Settings › Meko Cloud.
  const needsKey = precheck.error instanceof ApiError && precheck.error.code === "CLOUD_NOT_LINKED";
  const addKey = () => {
    onClose();
    navigate(MEKO_CLOUD);
  };

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
        ) : needsKey ? (
          <>
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button variant="primary" onClick={addKey}>
              <KeyRound className="size-4" /> Add API key
            </Button>
          </>
        ) : (
          <>
            <Button variant="ghost" onClick={onClose}>
              {syncing ? "Close" : "Cancel"}
            </Button>
            <Button variant="primary" disabled={!confirming || !confirmed} onClick={start}>
              <Cloud className="size-4" /> Sync to Meko Cloud
            </Button>
          </>
        )
      }
    >
      {done ? (
        <>
          <p className="text-sm">
            <b>{name}</b> lives in Meko Cloud now, and it's gone from this computer. Agents keep using it as before:
            every call on it goes to Meko Cloud.
            {run.started_at && ` Synced in ${clock(syncElapsed(run, now))}.`}
          </p>
          <SyncTimeline run={run} now={now} />
        </>
      ) : (
        <>
          <p className="text-sm text-muted-foreground">
            Everything in <b>{name}</b> moves to Meko Cloud in one go. Once it's there, it's deleted from this computer,
            and agents reach it in Meko Cloud with no change on their side. If anything goes wrong, it stays here as it
            is.
          </p>
          {needsKey && (
            <p className="text-sm">
              First link Meko Cloud with your API key, so Meko knows which account to move it to. Then sync it again.
            </p>
          )}
          {precheck.error && !needsKey && <ErrorNote error={precheck.error} />}
          {!precheck.data && !precheck.error && (
            <p className="flex items-center gap-2 text-sm text-muted-foreground">
              <Spinner className="size-3.5" /> Checking it fits your Meko Cloud plan…
            </p>
          )}
          {checks.length > 0 && !timeline && (
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
          {confirming && (
            <div className="space-y-3 rounded-lg border p-3">
              <label className="flex cursor-pointer items-start gap-2 text-sm">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={agreed}
                  onChange={(e) => setAgreed(e.target.checked)}
                />
                <span>
                  I understand that <b>{name}</b> moves to Meko Cloud and is deleted from this computer.
                </span>
              </label>
              <div className="space-y-1.5">
                <p className="text-sm">
                  Type <b>{name}</b> to confirm.
                </p>
                <Input value={typed} onChange={(e) => setTyped(e.target.value)} aria-label="Type the name to confirm" />
              </div>
            </div>
          )}
          {timeline && <SyncTimeline run={timeline} now={now} />}
          {syncing && (
            <p className="text-xs text-muted-foreground">
              Writes to it are refused until it's done. You can close this: the sync goes on.
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
