// Meko's settings: memory extraction and Search's answers, which run the user's own Claude Code (each
// turned on after one test call), usage stats, keeping calls' contents and the way to the Calls page,
// the way to the Meko Cloud link, and who can reach Meko.

import { useState, type ReactNode } from "react";
import { Cloud, ListTree } from "lucide-react";
import { api } from "../lib/api";
import { bytes, dateTime } from "../lib/format";
import { CALLS, href, navigate, type Route } from "../lib/route";
import type { AuthStatus, UsageStats } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { Button, Card, ErrorNote, Spinner, Toggle } from "../components/ui";

const HARNESS = "claude";

const MEKO_CLOUD = { tab: "overview" as const, view: "meko-cloud" as const };

const LINK_CLASS =
  "inline-flex h-7 items-center gap-1.5 rounded-lg border bg-card px-2 text-xs font-medium hover:bg-accent";

function usageNote(u: UsageStats): string | null {
  if (u.forced_off) return `Turned off by ${u.by}. Remove that setting and restart Meko to turn them on here.`;
  if (u.enabled && !u.sends) return "A build from source sends none.";
  return null;
}

export function SettingsPage({ auth }: { auth: AuthStatus }) {
  return (
    <div className="max-w-3xl space-y-4">
      <h2 className="text-lg font-semibold">Settings</h2>
      <ClaudeFeatures />
      <UsageStatsCard />
      <CallContentsCard />
      <Card className="space-y-2 p-4">
        <h3 className="font-medium">Meko Cloud</h3>
        <p className="text-sm text-muted-foreground">
          Link Meko Cloud for your extensions, such as AgentK, to keep their memory there instead of on this computer.
        </p>
        <PageLink to={MEKO_CLOUD} icon={<Cloud className="size-3.5" />} label="Meko Cloud" />
      </Card>
      <AccessCard auth={auth} />
    </div>
  );
}

// Who reaches this page: [web] access, local or remote.
function AccessCard({ auth }: { auth: AuthStatus }) {
  return (
    <Card className="space-y-2 p-4">
      <h3 className="font-medium">Access</h3>
      <p className="text-sm text-muted-foreground">{auth.access === "remote" ? "Remote" : "Local"}</p>
    </Card>
  );
}

function PageLink({ to, icon, label }: { to: Route; icon: ReactNode; label: string }) {
  return (
    <a href={href(to)} onClick={(e) => (e.preventDefault(), navigate(to))} className={LINK_CLASS}>
      {icon} {label}
    </a>
  );
}

// Memory extraction and Search's answers: each a switch, with how it's running when on. Turning one
// on waits for its test call, which can take a few seconds.
function ClaudeFeatures() {
  const extractor = useLoad(() => api.memoryExtractor(), []);
  const answers = useLoad(() => api.answers(), []);
  const x = extractor.data;
  return (
    <Card className="space-y-4 p-4">
      <div className="space-y-1">
        <h3 className="font-medium">Uses your Claude Code</h3>
        <p className="text-sm text-muted-foreground">
          Meko runs your own Claude Code in the background, with no tools, for these. Each call uses your Claude quota,
          so they're off until you turn them on. Codex and Cursor are coming.
        </p>
      </div>
      <Feature
        title="Memory extraction"
        about="Pulls memories out of your agents' conversations, from your own messages, every few minutes."
        loaded={extractor}
        set={async (on) => void (await api.setMemoryExtractor(on ? HARNESS : ""))}
        details={
          x?.enabled &&
          [
            `${x.harness} (${x.model}), every ${Math.round(x.interval_seconds / 60)} min`,
            `${x.queued} turns waiting`,
            `${x.extracted} memories extracted`,
            x.last_run_at ? `last run ${dateTime(x.last_run_at)}` : "not run yet",
          ].join(" · ")
        }
        failing={x?.enabled ? x.last_error : undefined}
      />
      <Feature
        title="Search answers"
        about="Writes an answer above Search's results, from those results only, citing them."
        loaded={answers}
        set={async (on) => void (await api.setAnswers(on ? HARNESS : ""))}
        details={answers.data?.enabled && `${answers.data.harness} (${answers.data.model})`}
      />
    </Card>
  );
}

function Feature({
  title,
  about,
  loaded,
  set,
  details,
  failing,
}: {
  title: string;
  about: string;
  loaded: { data?: { enabled: boolean }; error?: Error; reload: () => void };
  set: (on: boolean) => Promise<void>;
  details?: string | false;
  failing?: string;
}) {
  const [busy, setBusy] = useState<boolean>();
  const [error, setError] = useState<Error>();
  const flip = async (on: boolean) => {
    setBusy(on);
    setError(undefined);
    try {
      await set(on);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(undefined);
      loaded.reload();
    }
  };
  const on = loaded.data?.enabled ?? false;
  return (
    <div className="space-y-2 border-t pt-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-0.5">
          <p className="text-sm font-medium">{title}</p>
          <p className="text-sm text-muted-foreground">{about}</p>
        </div>
        <Toggle label={title} checked={on} disabled={!loaded.data || busy !== undefined} onChange={flip} />
      </div>
      {busy === true && (
        <p className="flex items-center gap-2 text-xs text-muted-foreground">
          <Spinner className="size-3.5" /> Checking that Claude Code answers…
        </p>
      )}
      {details && <p className="text-xs text-muted-foreground">{details}</p>}
      {failing && <p className="text-xs text-destructive">Last error: {failing}</p>}
      {(error ?? loaded.error) && <ErrorNote error={(error ?? loaded.error)!} />}
    </div>
  );
}

function UsageStatsCard() {
  const usage = useLoad(() => api.usageStats(), []);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const u = usage.data;
  const flip = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      await api.setUsageStats(on);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
      usage.reload();
    }
  };
  const hint = u && usageNote(u);
  return (
    <Card className="space-y-2 p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-0.5">
          <h3 className="font-medium">Usage stats</h3>
          <p className="text-sm text-muted-foreground">Send usage stats to YugabyteDB.</p>
          {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
        </div>
        {u ? (
          <Toggle label="Send usage stats" checked={u.enabled} disabled={busy || u.forced_off} onChange={flip} />
        ) : (
          !usage.error && <Spinner />
        )}
      </div>
      {(error ?? usage.error) && <ErrorNote error={(error ?? usage.error)!} />}
    </Card>
  );
}

// Keep call contents: what each MCP call sent and got back, kept for a few days for the Calls page,
// which this card is the way to. Clearing deletes what's kept; the calls stay.
function CallContentsCard() {
  const state = useLoad(() => api.callContents(), []);
  const [busy, setBusy] = useState(false);
  const [clearing, setClearing] = useState(false);
  const [error, setError] = useState<Error>();
  const s = state.data;
  const flip = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      await api.setCallContents(on);
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
      state.reload();
    }
  };
  return (
    <Card className="space-y-3 p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-0.5">
          <h3 className="font-medium">Calls</h3>
          <p className="text-sm text-muted-foreground">
            Keep what each call to Meko sent and got back, to see on the Calls page what your agents asked and what
            they were told.
          </p>
          {s && (
            <p className="text-xs text-muted-foreground">
              Kept for {s.retention_days} days. {s.calls ? `${s.calls} calls kept, ${bytes(s.bytes)}.` : "None kept now."}
            </p>
          )}
        </div>
        {s ? (
          <Toggle label="Keep call contents" checked={s.keep} disabled={busy} onChange={flip} />
        ) : (
          !state.error && <Spinner />
        )}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <PageLink to={CALLS} icon={<ListTree className="size-3.5" />} label="View calls" />
        {s && s.calls > 0 && (
          <Button size="sm" variant="ghost" onClick={() => setClearing(true)}>
            Clear contents
          </Button>
        )}
      </div>
      {(error ?? state.error) && <ErrorNote error={(error ?? state.error)!} />}
      {clearing && (
        <ConfirmDialog
          title="Clear call contents?"
          action="Clear"
          onClose={() => setClearing(false)}
          onConfirm={async () => {
            await api.clearCallContents();
            setClearing(false);
            state.reload();
          }}
        >
          What every call sent and got back is deleted. The calls stay on the Calls page.
        </ConfirmDialog>
      )}
    </Card>
  );
}
