// The Extensions page: the extensions installed (registered with Meko) and the ones available to
// install, each with Install, Start, Stop and Open, and for a release Meko installed, Update (while
// a newer release is out), Check now and Update automatically. An action runs on the server as the
// extension's job (an automatic update too, marked so); its card shows the output while it runs, and
// the error and output when it failed.

import { useState } from "react";
import { ExternalLink } from "lucide-react";
import { api } from "../lib/api";
import { isInstalled, notifyExtensionsChanged, useExtensions } from "../lib/extensions";
import type { Extension, ExtensionAction } from "../lib/types";
import { Dialog } from "../components/Dialog";
import { ExtensionIcon } from "../components/ExtensionsLink";
import { SectionHeader } from "../components/metrics/SectionHeader";
import { Badge, Button, Card, Code, ErrorNote, Loading, Spinner } from "../components/ui";
import { timeAgo } from "../lib/format";

const LABEL: Record<ExtensionAction, string> = { install: "Install", update: "Update", start: "Start", stop: "Stop" };
const DOING: Record<ExtensionAction, string> = {
  install: "Installing…",
  update: "Updating…",
  start: "Starting…",
  stop: "Stopping…",
};

export function ExtensionsPage() {
  const list = useExtensions();
  if (list.loading && !list.data) return <Loading />;
  if (!list.data) return list.error ? <ErrorNote error={list.error} /> : null;
  const installed = list.data.filter(isInstalled);
  const available = list.data.filter((e) => !isInstalled(e));
  return (
    <div className="max-w-4xl">
      <h1 className="text-xl font-semibold">Extensions</h1>
      <p className="mb-6 mt-1 text-sm text-muted-foreground">
        Apps that plug into Meko, served on its port behind its sign-in.
      </p>
      <SectionHeader label="Installed" />
      <Cards rows={installed} empty="No extension is installed yet." />
      <SectionHeader label="Available" />
      <Cards rows={available} empty="Every extension Meko knows is installed." />
    </div>
  );
}

function Cards({ rows, empty }: { rows: Extension[]; empty: string }) {
  if (rows.length === 0) return <p className="text-sm text-muted-foreground">{empty}</p>;
  return (
    <div className="space-y-3">
      {rows.map((e) => (
        <ExtensionCard key={e.name} ext={e} />
      ))}
    </div>
  );
}

function ExtensionCard({ ext }: { ext: Extension }) {
  const [posting, setPosting] = useState<ExtensionAction>();
  const [error, setError] = useState<Error>();
  // Stop and Update ask first: both end what the extension is doing.
  const [confirm, setConfirm] = useState<"stop" | "update">();
  const installed = isInstalled(ext);
  const webUI = ext.web_ui ?? true;
  const running = webUI && ext.available;
  const job = ext.job;
  const doing = job?.state === "running" ? job.action : posting;

  const [checking, setChecking] = useState(false);
  const [savingAuto, setSavingAuto] = useState(false);

  const check = async () => {
    setError(undefined);
    setChecking(true);
    try {
      await api.extensionCheck(ext.name);
      notifyExtensionsChanged();
    } catch (e) {
      setError(e as Error);
    } finally {
      setChecking(false);
    }
  };

  const setAuto = async (on: boolean) => {
    setError(undefined);
    setSavingAuto(true);
    try {
      await api.setExtensionAutoUpdate(ext.name, on);
      notifyExtensionsChanged();
    } catch (e) {
      setError(e as Error);
    } finally {
      setSavingAuto(false);
    }
  };

  const act = async (action: ExtensionAction) => {
    setConfirm(undefined);
    setError(undefined);
    setPosting(action);
    try {
      await api.extensionAction(ext.name, action);
      notifyExtensionsChanged();
    } catch (e) {
      setError(e as Error);
    } finally {
      setPosting(undefined);
    }
  };

  const button = (action: ExtensionAction, primary: boolean) => (
    <Button
      key={action}
      variant={primary ? "primary" : "outline"}
      disabled={doing !== undefined}
      onClick={() => (action === "stop" || action === "update" ? setConfirm(action) : act(action))}
    >
      {doing === action && <Spinner className="text-current" />}
      {doing === action ? DOING[action] : LABEL[action]}
    </Button>
  );

  return (
    <Card className="p-4">
      <div className="flex flex-wrap items-start gap-3">
        <div className="flex min-w-0 flex-1 items-start gap-3">
          {/* shrink-0: a logo is an img, which preflight gives max-width: 100%, so a long description would squeeze it */}
          <div className="shrink-0 rounded-lg bg-primary/10 p-2 text-primary">
            <ExtensionIcon name={ext.name} className="size-6" />
          </div>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium">{ext.title}</span>
              {installed && ext.version && <span className="text-xs text-muted-foreground">{ext.version}</span>}
              {installed && ext.update_available && ext.latest && <Badge tone="primary">{ext.latest} available</Badge>}
              {installed &&
                (!webUI ? (
                  <Badge>Installed</Badge>
                ) : running ? (
                  <Badge tone="success">Running</Badge>
                ) : (
                  <Badge>Not running</Badge>
                ))}
            </div>
            {ext.description && <p className="mt-1 break-words text-sm text-muted-foreground">{ext.description}</p>}
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap gap-2">
          {!installed && button("install", true)}
          {installed && !running && button("start", true)}
          {installed && (running || !webUI) && button("stop", false)}
          {/* Update only while there's a newer release (the badge's test), or while one installs. */}
          {installed && ext.updatable && (ext.update_available || doing === "update") && button("update", false)}
          {running && ext.path && (
            // A plain link, opening it in place of this page: AgentK refuses to be framed.
            <a
              href={ext.path}
              className="inline-flex h-9 items-center gap-1.5 rounded-lg bg-primary px-3 text-sm font-medium text-primary-foreground hover:opacity-90"
            >
              Open <ExternalLink className="size-3.5" />
            </a>
          )}
        </div>
      </div>

      {installed && ext.updatable && (
        <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
          <label className="flex items-center gap-2">
            <input
              id={`auto-update-${ext.name}`}
              type="checkbox"
              checked={ext.auto_update ?? true}
              disabled={savingAuto}
              onChange={(e) => void setAuto(e.target.checked)}
            />
            <span>Update automatically</span>
          </label>
          <Button variant="ghost" disabled={checking || doing !== undefined} onClick={() => void check()}>
            {checking && <Spinner className="text-current" />}
            {checking ? "Checking…" : "Check now"}
          </Button>
          <span className="text-xs text-muted-foreground">{updateNote(ext)}</span>
        </div>
      )}
      {installed && (
        <p className="mt-3 break-words text-xs text-muted-foreground">
          <Code>meko extension delete {ext.name}</Code> stops and removes it; its data stays unless you add{" "}
          <Code>--purge</Code>.
        </p>
      )}
      {error && (
        <div className="mt-3">
          <ErrorNote error={error} />
        </div>
      )}
      {job?.state === "failed" && (
        <div className="mt-3">
          <ErrorNote
            error={new Error(`${job.auto ? "The automatic update" : LABEL[job.action]} failed: ${job.error ?? "unknown error"}`)}
          />
        </div>
      )}
      {job?.action === "update" && job.state === "done" && job.output && (
        // A finished update says what it did: up to date, or installed for the next start.
        <p className="mt-3 break-words text-sm text-muted-foreground">{lastLine(job.output)}</p>
      )}
      {job && job.state !== "done" && job.output && (
        <pre className="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted p-3 font-mono text-xs">
          {job.output}
        </pre>
      )}

      {confirm === "stop" && (
        <Dialog
          title={`Stop ${ext.title}?`}
          onClose={() => setConfirm(undefined)}
          actions={
            <>
              <Button variant="ghost" onClick={() => setConfirm(undefined)}>
                Cancel
              </Button>
              <Button variant="primary" className="bg-destructive text-white" onClick={() => act("stop")}>
                Stop {ext.title}
              </Button>
            </>
          }
        >
          <p className="text-sm">
            {ext.name === "agentk"
              ? "Stopping AgentK ends every session running in it. They resume when it starts again."
              : `${ext.title} stops answering until it starts again.`}
          </p>
        </Dialog>
      )}
      {confirm === "update" && (
        <Dialog
          title={`Update ${ext.title}?`}
          onClose={() => setConfirm(undefined)}
          actions={
            <>
              <Button variant="ghost" onClick={() => setConfirm(undefined)}>
                Cancel
              </Button>
              <Button variant="primary" onClick={() => act("update")}>
                Update {ext.title}
              </Button>
            </>
          }
        >
          <p className="text-sm">
            {ext.name === "agentk"
              ? "Meko installs AgentK's newest release and, if it's running, restarts its server: sessions keep running, and open pages reconnect."
              : `Meko installs ${ext.title}'s newest release and, if it's running, restarts it: open pages reconnect, and a live talk resumes where it was.`}{" "}
            A release that fails to start is rolled back.
          </p>
        </Dialog>
      )}
    </Card>
  );
}

// What the card says about updates: when the last check ran (or why it failed), an update on
// probation, and why an automatic one waits.
function updateNote(ext: Extension): string {
  const parts: string[] = [];
  if (ext.check_error) parts.push(`The last check failed: ${ext.check_error}`);
  else if (ext.checked_at) parts.push(`Checked ${timeAgo(ext.checked_at)}`);
  if (ext.pending) parts.push(`${ext.pending.to} is new: it counts as installed once it has run 5 minutes`);
  if (ext.waiting) parts.push(`The automatic update waits: ${ext.waiting}`);
  return parts.join(". ");
}

// The last line of a job's output.
function lastLine(output: string): string {
  const lines = output.trim().split("\n");
  return lines[lines.length - 1] ?? "";
}
