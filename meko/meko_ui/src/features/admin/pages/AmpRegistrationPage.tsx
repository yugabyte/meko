// AMP section: register the AMP control-plane endpoint for this Meko deployment
// (global singleton). api_server reads this to provision clusters inside
// datapacks. The password is write-only (never returned to the client).
import { useEffect, useState } from "react";
import { Database, Loader2, Trash2 } from "lucide-react";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { ADMIN_API_ERROR_HINT } from "@/features/admin/adminCopy";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  getAmpRegistration,
  registerAmp,
  updateAmpRegistration,
  unregisterAmp,
  type AmpRegistration,
} from "@/lib/api/adminApi";

export function AmpRegistrationPage() {
  const [current, setCurrent] = useState<AmpRegistration | null>(null);
  const [loading, setLoading] = useState(true);
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  // Distinct from `current === null`: a load *failure* (network/timeout/401)
  // must not be shown as the genuine "not registered" empty state, or the page
  // would offer Register (POST) when a registration may already exist. 404 is
  // mapped to `current === null` by getAmpRegistration and is NOT an error here.
  const [loadError, setLoadError] = useState<string | null>(null);
  // Unregister is destructive (drops the singleton AMP endpoint, breaking
  // cluster provisioning) — gate it behind a confirmation dialog.
  const [confirmUnregister, setConfirmUnregister] = useState(false);

  const load = async () => {
    setLoading(true);
    setLoadError(null);
    try {
      const reg = await getAmpRegistration();
      setCurrent(reg);
      if (reg) {
        setName(reg.name);
        setUrl(reg.url);
        setUsername(reg.username ?? "");
      }
    } catch (e) {
      setLoadError(getApiErrorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!url.trim()) return;
    setSubmitting(true);
    try {
      let reg: AmpRegistration;
      if (current) {
        // Update: PATCH so an empty password leaves the stored one untouched.
        const patch: Partial<{ name: string; url: string; username: string; password: string }> = {
          name,
          url,
          username,
        };
        if (password) patch.password = password;
        reg = await updateAmpRegistration(patch);
      } else {
        reg = await registerAmp({ name, url, username, password });
      }
      setCurrent(reg);
      setPassword("");
      toast({ title: current ? "AMP updated" : "AMP registered", description: reg.url });
    } catch (err) {
      toast({
        variant: "destructive",
        title: "Could not save AMP registration",
        description: getApiErrorMessage(err),
      });
    } finally {
      setSubmitting(false);
    }
  };

  const unregister = async () => {
    setSubmitting(true);
    try {
      await unregisterAmp();
      setCurrent(null);
      setName("");
      setUrl("");
      setUsername("");
      setPassword("");
      toast({ title: "AMP unregistered" });
    } catch (e) {
      toast({
        variant: "destructive",
        title: "Could not unregister AMP",
        description: getApiErrorMessage(e),
      });
    } finally {
      setSubmitting(false);
      setConfirmUnregister(false);
    }
  };

  return (
    <div className="flex-1 overflow-y-auto p-8">
      <div className="mx-auto w-full max-w-2xl space-y-6">
        <header className="space-y-1">
          <h1 className="flex items-center gap-2 text-lg font-semibold tracking-tight">
            <Database className="h-5 w-5 text-orange-400" aria-hidden />
            AMP
          </h1>
          <p className="text-sm text-muted-foreground">
            Register the AMP control-plane endpoint for this Meko deployment. Clusters created
            inside datapacks are provisioned through it.
          </p>
        </header>

        {loading ? (
          <div className="flex items-center gap-2 text-sm text-muted-foreground">
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            Loading…
          </div>
        ) : loadError ? (
          <div className="space-y-3">
            <Alert variant="destructive">
              <AlertDescription>
                Failed to load AMP registration. {ADMIN_API_ERROR_HINT}
              </AlertDescription>
            </Alert>
            <Button type="button" variant="outline" onClick={() => void load()}>
              Retry
            </Button>
          </div>
        ) : (
          <section className="rounded-xl border border-border bg-card p-5">
            <p className="mb-4 text-xs text-muted-foreground">
              {current ? (
                <>
                  Currently registered:{" "}
                  <span className="font-mono text-foreground">{current.url}</span>
                </>
              ) : (
                "Not registered yet."
              )}
            </p>

            <form onSubmit={submit} className="space-y-4">
              <div className="space-y-1.5">
                <Label htmlFor="amp-name">Name</Label>
                <Input
                  id="amp-name"
                  placeholder="e.g. local or amp-dev"
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="amp-url">Controller URL</Label>
                <Input
                  id="amp-url"
                  placeholder="http://localhost:8080"
                  value={url}
                  onChange={(e) => setUrl(e.target.value)}
                />
              </div>
              <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                <div className="space-y-1.5">
                  <Label htmlFor="amp-username">Username (optional)</Label>
                  <Input
                    id="amp-username"
                    value={username}
                    onChange={(e) => setUsername(e.target.value)}
                  />
                </div>
                <div className="space-y-1.5">
                  <Label htmlFor="amp-password">Password (optional)</Label>
                  <Input
                    id="amp-password"
                    type="password"
                    placeholder={current ? "••••••" : ""}
                    value={password}
                    onChange={(e) => setPassword(e.target.value)}
                  />
                </div>
              </div>

              <div className="flex flex-wrap items-center gap-2 pt-1">
                <Button type="submit" disabled={submitting || !url.trim()}>
                  {submitting ? "Saving…" : current ? "Update registration" : "Register AMP"}
                </Button>
                {current ? (
                  <Button
                    type="button"
                    variant="outline"
                    className="gap-2 text-destructive"
                    disabled={submitting}
                    onClick={() => setConfirmUnregister(true)}
                  >
                    <Trash2 className="h-4 w-4" aria-hidden />
                    Unregister
                  </Button>
                ) : null}
              </div>
            </form>
          </section>
        )}

        {!loading && current && (
          <section className="rounded-xl border border-border bg-card p-5 text-sm text-muted-foreground">
            To view the shared pools and the cluster instances spun up, visit{" "}
            <a
              href={current.url}
              target="_blank"
              rel="noreferrer"
              className="font-medium text-orange-400 underline underline-offset-2 hover:text-orange-300"
            >
              {current.url}
            </a>
          </section>
        )}
      </div>

      <AlertDialog
        open={confirmUnregister}
        onOpenChange={(o) => !o && !submitting && setConfirmUnregister(false)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Unregister AMP?</AlertDialogTitle>
            <AlertDialogDescription>
              This removes the AMP control-plane endpoint for this Meko deployment. Cluster
              provisioning inside datapacks will stop working until AMP is registered again.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={submitting}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={submitting}
              onClick={(e) => {
                e.preventDefault();
                void unregister();
              }}
            >
              Unregister
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
