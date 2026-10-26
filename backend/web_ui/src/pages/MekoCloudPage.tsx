// Settings › Meko Cloud at /settings/meko-cloud: the Meko Cloud (or another Meko server) this Meko's
// extensions use, and its API key. Meko checks a new server or key before saving it, and tells the
// running extensions; Unlink sends them back to this Meko. AgentK sets each coding agent up with the
// link in its own Settings › Harness.

import { useEffect, useState, type FormEvent } from "react";
import { api } from "../lib/api";
import { notifyCloudChanged } from "../lib/cloud";
import { httpHost, mekoWebUrl } from "../lib/mekourls";
import type { MekoCloud, MekoCloudUpdate } from "../lib/types";
import { MekoGetKey } from "../components/MekoGetKey";
import { Badge, Button, Card, Code, ErrorNote, Field, Input, Loading, Spinner } from "../components/ui";

export function MekoCloudPage() {
  const [cloud, setCloud] = useState<MekoCloud>();
  const [loadError, setLoadError] = useState<Error>();
  useEffect(() => {
    api.mekoCloud().then(setCloud, setLoadError);
  }, []);
  return (
    <div className="max-w-2xl space-y-4">
      <div className="space-y-1">
        <h1 className="text-xl font-semibold">Meko Cloud</h1>
        <p className="text-sm text-muted-foreground">
          The Meko Cloud your extensions use for their memory, in place of this Meko. AgentK keeps each project's datapack
          there, and shares projects through it.
        </p>
      </div>
      {cloud ? <LinkForm cloud={cloud} onSaved={setCloud} /> : loadError ? <ErrorNote error={loadError} /> : <Loading />}
      <p className="text-xs text-muted-foreground">
        On a computer without a browser, <Code>meko cloud_link</Code> does the same. Each coding agent's own Meko setup is
        in AgentK's Settings › Harness.
      </p>
    </div>
  );
}

function LinkForm({ cloud, onSaved }: { cloud: MekoCloud; onSaved: (c: MekoCloud) => void }) {
  const [url, setUrl] = useState(cloud.url);
  const [key, setKey] = useState("");
  const [webUrl, setWebUrl] = useState(cloud.web_url);
  const [apiUrl, setApiUrl] = useState(cloud.api_url);
  const [own, setOwn] = useState(Boolean(cloud.url || cloud.web_url || cloud.api_url));
  // The host a save would switch to, asked about once before it's saved.
  const [confirmHost, setConfirmHost] = useState<string | null>(null);
  const [confirmUnlink, setConfirmUnlink] = useState(false);
  const [busy, setBusy] = useState<"save" | "unlink" | null>(null);
  const [error, setError] = useState<Error>();
  const [saved, setSaved] = useState<string | null>(null);

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

  const save = async (e: FormEvent) => {
    e.preventDefault();
    const next: MekoCloudUpdate = {};
    if (url.trim() !== cloud.url) next.url = url.trim();
    if (webUrl.trim() !== cloud.web_url) next.web_url = webUrl.trim();
    if (apiUrl.trim() !== cloud.api_url) next.api_url = apiUrl.trim();
    if (key.trim()) next.api_key = key.trim();
    if (cloud.linked && !Object.keys(next).length) return;
    // Datapacks live on one Meko: moving to another server leaves existing projects' behind.
    const newHost = httpHost(typedUrl);
    if (cloud.linked && next.url !== undefined && newHost && newHost !== currentHost && confirmHost !== newHost) {
      setConfirmHost(newHost);
      return;
    }
    setBusy("save");
    setError(undefined);
    setSaved(null);
    try {
      show(await api.linkMekoCloud(next));
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
    setBusy("unlink");
    setError(undefined);
    setSaved(null);
    try {
      show(await api.unlinkMekoCloud());
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
      <form className="space-y-4" onSubmit={save}>
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
        <button
          type="button"
          className="text-xs font-medium text-primary hover:underline"
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
            be found after the switch. Press Switch to switch anyway.
          </p>
        )}
        {error && <ErrorNote error={error} />}
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" variant="primary" disabled={busy !== null}>
            {busy === "save" ? <Spinner className="text-primary-foreground" /> : null}
            {busy === "save" ? "Checking…" : confirmHost ? "Switch" : cloud.linked ? "Check and save" : "Check and link"}
          </Button>
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
