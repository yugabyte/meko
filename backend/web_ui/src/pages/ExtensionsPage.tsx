// The Extensions page: the extensions installed (registered with Meko) and the ones available to
// install, each with Install, Start, Stop and Open. An action runs on the server as the extension's
// job; its card shows the output while it runs, and the error and output when it failed.

import { useState } from "react";
import { ExternalLink } from "lucide-react";
import { api } from "../lib/api";
import { isInstalled, notifyExtensionsChanged, useExtensions } from "../lib/extensions";
import type { Extension, ExtensionAction } from "../lib/types";
import { Dialog } from "../components/Dialog";
import { ExtensionIcon } from "../components/ExtensionsLink";
import { SectionHeader } from "../components/metrics/SectionHeader";
import { Badge, Button, Card, Code, ErrorNote, Loading, Spinner } from "../components/ui";

const LABEL: Record<ExtensionAction, string> = { install: "Install", start: "Start", stop: "Stop" };
const DOING: Record<ExtensionAction, string> = { install: "Installing…", start: "Starting…", stop: "Stopping…" };

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
  const [confirmStop, setConfirmStop] = useState(false);
  const installed = isInstalled(ext);
  const webUI = ext.web_ui ?? true;
  const running = webUI && ext.available;
  const job = ext.job;
  const doing = job?.state === "running" ? job.action : posting;

  const act = async (action: ExtensionAction) => {
    setConfirmStop(false);
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
      onClick={() => (action === "stop" ? setConfirmStop(true) : act(action))}
    >
      {doing === action && <Spinner className="text-current" />}
      {doing === action ? DOING[action] : LABEL[action]}
    </Button>
  );

  return (
    <Card className="p-4">
      <div className="flex flex-wrap items-start gap-3">
        <div className="flex min-w-0 flex-1 items-start gap-3">
          <div className="rounded-lg bg-primary/10 p-2 text-primary">
            <ExtensionIcon name={ext.name} className="size-5" />
          </div>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="font-medium">{ext.title}</span>
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

      {installed && (
        <p className="mt-3 break-words text-xs text-muted-foreground">
          <Code>meko uninstall_extension {ext.name}</Code> stops Meko starting and stopping it.
        </p>
      )}
      {error && (
        <div className="mt-3">
          <ErrorNote error={error} />
        </div>
      )}
      {job?.state === "failed" && (
        <div className="mt-3">
          <ErrorNote error={new Error(`${LABEL[job.action]} failed: ${job.error ?? "unknown error"}`)} />
        </div>
      )}
      {job && job.state !== "done" && job.output && (
        <pre className="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded-lg border bg-muted p-3 font-mono text-xs">
          {job.output}
        </pre>
      )}

      {confirmStop && (
        <Dialog
          title={`Stop ${ext.title}?`}
          onClose={() => setConfirmStop(false)}
          actions={
            <>
              <Button variant="ghost" onClick={() => setConfirmStop(false)}>
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
    </Card>
  );
}
