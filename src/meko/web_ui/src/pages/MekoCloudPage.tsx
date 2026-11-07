// Settings › Meko Cloud at /settings/meko-cloud: the Meko Cloud (or another Meko server) this Meko's
// extensions use. Signing in with the browser links it: Meko's server starts the sign-in, Meko Cloud
// sends the browser back to /settings/meko-cloud/callback (this page), and the server makes an API
// key and links with it. An API key can be pasted instead. Meko checks a new server or key before
// saving it, and tells the running extensions; Unlink sends them back to this Meko. Agents stay on
// this Meko, which sends their calls to Meko Cloud while it's linked: a change moves back the agents a
// Meko from before connected to Meko Cloud itself, and the page says which moved (or couldn't).

import { useEffect, useState, type FormEvent } from "react";
import { ExternalLink } from "lucide-react";
import { api } from "../lib/api";
import { notifyCloudChanged } from "../lib/cloud";
import { httpHost, mekoKeyLinks, mekoWebUrl } from "../lib/mekourls";
import type { AgentMoves, MekoCloud, MekoCloudUpdate } from "../lib/types";
import { MekoGetKey } from "../components/MekoGetKey";
import { Badge, Button, Card, Code, ErrorNote, Field, Input, Loading, Spinner } from "../components/ui";

const PAGE = "/settings/meko-cloud";
const CALLBACK = `${PAGE}/callback`;

// What a link change did, shown under the form: a line that it worked, what it did to the agents, or
// why it failed.
interface Outcome {
  saved?: string;
  moves?: { text: string; failed: boolean } | null;
  error?: Error;
}

// The sign-in Meko Cloud sent the browser back with, finished once by its state: React may run an
// effect twice.
let finishing: { state: string; done: Promise<MekoCloud> } | null = null;

// What the address says when Meko Cloud sent the browser back here: null on the page itself. Read
// while rendering, before the effect takes the code out of the address, so a second run of the
// effect (React's StrictMode) sees the same sign-in rather than an address without one.
type Back = { code: string; state: string } | { refused: string } | null;

function readBack(): Back {
  if (window.location.pathname !== CALLBACK) return null;
  const q = new URLSearchParams(window.location.search);
  const code = q.get("code");
  const state = q.get("state");
  const refused = q.get("error");
  if (!refused && code && state) return { code, state };
  return { refused: (q.get("error_description") || refused || "it sent no sign-in code back").replace(/\.$/, "") };
}

export function MekoCloudPage() {
  const [cloud, setCloud] = useState<MekoCloud>();
  const [loadError, setLoadError] = useState<Error>();
  const [outcome, setOutcome] = useState<Outcome>();
  const [back] = useState(readBack);
  useEffect(() => {
    if (!back) {
      api.mekoCloud().then(setCloud, setLoadError);
      return;
    }
    // Back from signing in at Meko Cloud: take the code out of the address, then finish.
    if (window.location.pathname === CALLBACK) history.replaceState(null, "", PAGE);
    if ("refused" in back) {
      setOutcome({ error: new Error(`Meko Cloud didn't sign you in: ${back.refused}. Nothing was saved.`) });
      api.mekoCloud().then(setCloud, setLoadError);
      return;
    }
    if (finishing?.state !== back.state) {
      finishing = { state: back.state, done: api.finishMekoCloudSignIn(back.code, back.state) };
    }
    finishing.done.then(
      (c) => {
        setCloud(c);
        setOutcome({ saved: "Signed in and linked. The running extensions use it now.", moves: movesLine(c.agents) });
        notifyCloudChanged();
      },
      (err: Error) => {
        setOutcome({ error: err });
        api.mekoCloud().then(setCloud, setLoadError);
      },
    );
  }, [back]);
  return (
    <div className="max-w-2xl space-y-4">
      <div className="space-y-1">
        <h1 className="text-xl font-semibold">Meko Cloud</h1>
        <p className="text-sm text-muted-foreground">
          The Meko Cloud your agents and extensions use for their memory. While it's linked, new datapacks are made there,
          and the ones already on this Meko keep working. AgentK keeps each new project's datapack there, and shares
          projects through it.
        </p>
      </div>
      {cloud ? (
        <LinkForm cloud={cloud} onSaved={setCloud} initial={outcome} />
      ) : loadError ? (
        <ErrorNote error={loadError} />
      ) : (
        <Loading />
      )}
      <p className="text-xs text-muted-foreground">
        On a computer without a browser, <Code>meko cloud-link</Code> does the same. The Agents page connects
        agents.
      </p>
    </div>
  );
}

function LinkForm({ cloud, onSaved, initial }: { cloud: MekoCloud; onSaved: (c: MekoCloud) => void; initial?: Outcome }) {
  const [url, setUrl] = useState(cloud.url);
  const [key, setKey] = useState("");
  const [webUrl, setWebUrl] = useState(cloud.web_url);
  const [apiUrl, setApiUrl] = useState(cloud.api_url);
  const [own, setOwn] = useState(Boolean(cloud.url || cloud.web_url || cloud.api_url));
  const [paste, setPaste] = useState(false);
  // The host a save or sign-in would switch to, asked about once before it goes ahead.
  const [confirmHost, setConfirmHost] = useState<string | null>(null);
  const [confirmUnlink, setConfirmUnlink] = useState(false);
  const [busy, setBusy] = useState<"save" | "unlink" | "signin" | null>(null);
  const [error, setError] = useState<Error | undefined>(initial?.error);
  const [saved, setSaved] = useState<string | null>(initial?.saved ?? null);
  // What the last link change did to the agents, when it moved one or failed to.
  const [moves, setMoves] = useState<{ text: string; failed: boolean } | null>(initial?.moves ?? null);

  useEffect(() => {
    if (!initial) return;
    setError(initial.error);
    setSaved(initial.saved ?? null);
    setMoves(initial.moves ?? null);
  }, [initial]);

  const show = (c: MekoCloud) => {
    onSaved(c);
    setUrl(c.url);
    setWebUrl(c.web_url);
    setApiUrl(c.api_url);
    setKey("");
  };

  const typedUrl = url.trim().replace(/\/+$/, "") || cloud.default_url;
  const currentHost = httpHost(cloud.effective_url);
  const keyLinksBase = mekoWebUrl(typedUrl, webUrl, cloud);
  const signup = mekoKeyLinks(keyLinksBase)?.signup;

  // The URL fields that changed, for a link or a sign-in.
  const urlChanges = (): MekoCloudUpdate => {
    const next: MekoCloudUpdate = {};
    if (url.trim() !== cloud.url) next.url = url.trim();
    if (webUrl.trim() !== cloud.web_url) next.web_url = webUrl.trim();
    if (apiUrl.trim() !== cloud.api_url) next.api_url = apiUrl.trim();
    return next;
  };

  // Datapacks live on one Meko: moving to another server leaves existing projects' behind, so a switch
  // is confirmed with a second press. True when it still needs that press.
  const needsConfirm = (next: MekoCloudUpdate) => {
    const newHost = httpHost(typedUrl);
    if (cloud.linked && next.url !== undefined && newHost && newHost !== currentHost && confirmHost !== newHost) {
      setConfirmHost(newHost);
      return true;
    }
    return false;
  };

  const begin = (what: "save" | "unlink" | "signin") => {
    setBusy(what);
    setError(undefined);
    setSaved(null);
    setMoves(null);
  };

  const signIn = async () => {
    const next = urlChanges();
    if (needsConfirm(next)) return;
    begin("signin");
    try {
      const { authorize_url } = await api.startMekoCloudSignIn(next);
      window.location.assign(authorize_url);
    } catch (err) {
      setError(err as Error);
      setBusy(null);
    }
  };

  const save = async (e: FormEvent) => {
    e.preventDefault();
    const next = urlChanges();
    if (key.trim()) next.api_key = key.trim();
    if (cloud.linked && !Object.keys(next).length) return;
    if (needsConfirm(next)) return;
    begin("save");
    try {
      const c = await api.linkMekoCloud(next);
      show(c);
      setMoves(movesLine(c.agents));
      notifyCloudChanged();
      setConfirmHost(null);
      setSaved("Linked. The running extensions use it now.");
    } catch (err) {
      setError(err as Error);
    } finally {
      setBusy(null);
    }
  };

  const unlink = async () => {
    begin("unlink");
    try {
      const c = await api.unlinkMekoCloud();
      show(c);
      setMoves(movesLine(c.agents));
      notifyCloudChanged();
      setConfirmUnlink(false);
      setSaved("Unlinked. The extensions use this Meko now.");
    } catch (err) {
      setError(err as Error);
    } finally {
      setBusy(null);
    }
  };

  return (
    <Card className="space-y-4 p-4 sm:p-5">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        {cloud.linked ? (
          <>
            <Badge tone="success">Linked</Badge>
            <span>
              to <span className="font-medium">{currentHost ?? cloud.effective_url}</span>
              {cloud.key_hint ? <span className="text-muted-foreground">, key ending in {cloud.key_hint}</span> : null}
            </span>
          </>
        ) : (
          <>
            <Badge>Not linked</Badge>
            <span className="text-muted-foreground">The extensions use this Meko.</span>
          </>
        )}
      </div>
      <div className="space-y-2">
        <p className="text-sm text-muted-foreground">
          {cloud.linked
            ? "Sign in again for a new key, or to link another account."
            : "Sign in to the Meko Cloud account your agents should use. Meko keeps an API key for this computer, never your password."}
        </p>
        <div className="flex flex-wrap items-center gap-3">
          <Button
            type="button"
            variant={cloud.linked ? "outline" : "primary"}
            disabled={busy !== null}
            onClick={() => void signIn()}
          >
            {busy === "signin" ? <Spinner className={cloud.linked ? undefined : "text-primary-foreground"} /> : null}
            {busy === "signin"
              ? "Opening Meko Cloud…"
              : confirmHost && !paste
                ? "Switch and sign in"
                : cloud.linked
                  ? "Sign in again"
                  : "Sign in with Meko Cloud"}
          </Button>
          {!cloud.linked && signup && (
            <span className="text-sm text-muted-foreground">
              No Meko Cloud account?{" "}
              <a
                className="inline-flex items-center gap-1 font-medium text-primary hover:underline"
                href={signup}
                target="_blank"
                rel="noreferrer"
              >
                Sign up <ExternalLink className="size-3.5" />
              </a>
            </span>
          )}
        </div>
      </div>
      <form className="space-y-4" onSubmit={save}>
        <button
          type="button"
          className="text-xs font-medium text-primary hover:underline"
          aria-expanded={paste}
          onClick={() => setPaste(!paste)}
        >
          {paste ? "▾" : "▸"} Or paste an API key
        </button>
        {paste && (
          <div className="space-y-4">
            {!cloud.linked && (
              <MekoGetKey
                webUrl={keyLinksBase}
                fallback={`Fill in the web app URL below to get links to sign up and make a key on ${httpHost(typedUrl) ?? "this Meko"}.`}
              />
            )}
            <Field
              label="API key"
              hint={
                cloud.linked
                  ? "Type a new key to replace the saved one. The key is never shown."
                  : "Paste the key you generated in Meko Cloud."
              }
            >
              <Input
                id="meko-cloud-key"
                type="password"
                autoComplete="off"
                className="font-mono"
                placeholder={cloud.linked ? `••••••••${cloud.key_hint ?? ""}` : "mko_tkn_…"}
                value={key}
                onChange={(e) => setKey(e.target.value)}
              />
            </Field>
          </div>
        )}
        <button
          type="button"
          className="block text-xs font-medium text-primary hover:underline"
          aria-expanded={own}
          onClick={() => setOwn(!own)}
        >
          {own ? "▾" : "▸"} Your own Meko server: MCP, web app and API URLs
        </button>
        {own && (
          <div className="space-y-4">
            <Field label="MCP server URL" hint={`Empty means Meko Cloud (${cloud.default_url}).`}>
              <Input
                id="meko-cloud-url"
                className="font-mono"
                placeholder={cloud.default_url}
                value={url}
                onChange={(e) => {
                  setUrl(e.target.value);
                  setConfirmHost(null);
                }}
              />
            </Field>
            <div className="grid gap-4 sm:grid-cols-2">
              <Field label="Web app URL" hint="For links to datapacks and sharing. Empty works it out for Meko Cloud.">
                <Input
                  id="meko-cloud-web"
                  className="font-mono"
                  placeholder={cloud.effective_web_url ?? "https://meko.example.com"}
                  value={webUrl}
                  onChange={(e) => setWebUrl(e.target.value)}
                />
              </Field>
              <Field label="API URL" hint="Meko's REST API. Empty is the web app's plus /api/v1.">
                <Input
                  id="meko-cloud-api"
                  className="font-mono"
                  placeholder={cloud.effective_api_url ?? "https://meko.example.com/api/v1"}
                  value={apiUrl}
                  onChange={(e) => setApiUrl(e.target.value)}
                />
              </Field>
            </div>
          </div>
        )}
        {cloud.linked && !cloud.effective_web_url && (
          <p className="rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm" role="status">
            The web app for {currentHost ?? "this server"} isn't known, so extensions can't link to datapacks
            {cloud.effective_api_url ? "" : " or share them"}. Set the web app URL to turn that on.
          </p>
        )}
        {confirmHost && (
          <p className="rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm" role="alert">
            Existing projects' datapacks live on {currentHost ?? "the current server"}, not on {confirmHost}, so they won't
            be found after the switch. Press {paste ? "Switch" : "Switch and sign in"} to switch anyway.
          </p>
        )}
        {error && <ErrorNote error={error} />}
        <div className="flex flex-wrap items-center gap-2">
          {(paste || (cloud.linked && own)) && (
            <Button type="submit" variant="primary" disabled={busy !== null}>
              {busy === "save" ? <Spinner className="text-primary-foreground" /> : null}
              {busy === "save" ? "Checking…" : confirmHost ? "Switch" : cloud.linked ? "Check and save" : "Check and link"}
            </Button>
          )}
          {cloud.linked &&
            (confirmUnlink ? (
              <>
                <Button type="button" variant="danger" disabled={busy !== null} onClick={() => void unlink()}>
                  {busy === "unlink" ? "Unlinking…" : "Unlink"}
                </Button>
                <Button type="button" variant="ghost" onClick={() => setConfirmUnlink(false)}>
                  Cancel
                </Button>
              </>
            ) : (
              <Button type="button" variant="ghost" disabled={busy !== null} onClick={() => setConfirmUnlink(true)}>
                Unlink…
              </Button>
            ))}
          {saved && <span className="text-sm text-muted-foreground">{saved}</span>}
        </div>
        {moves && (
          <p
            className={
              moves.failed
                ? "rounded-lg border border-warning/40 bg-warning/10 p-3 text-sm"
                : "text-sm text-muted-foreground"
            }
            role="status"
          >
            {moves.text}
          </p>
        )}
        {confirmUnlink && (
          <p className="text-sm text-muted-foreground" role="alert">
            The extensions switch to this Meko. Projects whose datapacks live on {currentHost ?? "Meko Cloud"} won't find
            them until you link it again.
          </p>
        )}
      </form>
    </Card>
  );
}

// One line for what a link change did to the agents: the ones it moved back to this Meko, then the
// ones it couldn't move and why; null when it changed none (or the server is older).
function movesLine(agents: AgentMoves | undefined): { text: string; failed: boolean } | null {
  const parts: string[] = [];
  if (agents?.moved?.length) parts.push(`Moved ${names(agents.moved.map((a) => a.title))} back to this Meko`);
  for (const f of agents?.failed ?? []) parts.push(`Couldn't move ${f.title}: ${f.error}`);
  return parts.length ? { text: parts.join(". "), failed: Boolean(agents?.failed?.length) } : null;
}

// "A", "A and B", "A, B and C".
function names(titles: string[]): string {
  return titles.length < 2 ? titles.join("") : `${titles.slice(0, -1).join(", ")} and ${titles[titles.length - 1]}`;
}
