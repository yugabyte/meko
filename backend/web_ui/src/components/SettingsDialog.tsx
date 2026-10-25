// Meko's settings: whether usage stats go to YugabyteDB, saved as soon as it's changed (turned off
// by config.toml or the environment, the checkbox is off and can't be turned on here), and the ways
// to the Meko Cloud link at /settings/meko-cloud and the account settings at /account.

import { useEffect, useState } from "react";
import { Cloud, UserRound } from "lucide-react";
import { api } from "../lib/api";
import { href, navigate } from "../lib/route";
import type { AuthStatus, UsageStats } from "../lib/types";
import { hasAccount, LocalAccessNote } from "../pages/AccountPage";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Spinner } from "./ui";

function note(u: UsageStats): string | null {
  if (u.forced_off) return `Turned off by ${u.by}. Remove that setting and restart Meko to turn them on here.`;
  if (u.enabled && !u.sends) return "A build from source sends none.";
  return null;
}

const ACCOUNT = { tab: "overview" as const, view: "account" as const };
const MEKO_CLOUD = { tab: "overview" as const, view: "meko-cloud" as const };

const LINK_CLASS =
  "inline-flex h-7 items-center gap-1.5 rounded-lg border bg-card px-2 text-xs font-medium hover:bg-accent";

export function SettingsDialog({ auth, onClose }: { auth: AuthStatus; onClose: () => void }) {
  const [usage, setUsage] = useState<UsageStats>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();

  useEffect(() => {
    api.usageStats().then(setUsage, setError);
  }, []);

  const toggle = async (on: boolean) => {
    setBusy(true);
    setError(undefined);
    try {
      setUsage(await api.setUsageStats(on));
    } catch (e) {
      setError(e as Error);
    } finally {
      setBusy(false);
    }
  };

  const hint = usage && note(usage);
  return (
    <Dialog
      title="Settings"
      onClose={onClose}
      actions={
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      }
    >
      {!usage && !error && <Spinner />}
      {usage && (
        <label className="flex items-start gap-2 text-sm">
          <input
            id="usage-stats"
            type="checkbox"
            className="mt-0.5"
            checked={usage.enabled}
            disabled={busy || usage.forced_off}
            onChange={(e) => toggle(e.target.checked)}
          />
          <span className="space-y-0.5">
            <span className="block font-medium">Send usage stats to YugabyteDB</span>
            {hint && <span className="block text-xs text-muted-foreground">{hint}</span>}
          </span>
        </label>
      )}
      {error && <ErrorNote error={error} />}
      <div className="space-y-2 border-t pt-3">
        <p className="text-sm font-medium">Meko Cloud</p>
        <p className="text-xs text-muted-foreground">
          Link Meko Cloud for your extensions, such as AgentK, to keep their memory there instead of on this computer.
        </p>
        <a
          href={href(MEKO_CLOUD)}
          onClick={(e) => {
            e.preventDefault();
            onClose();
            navigate(MEKO_CLOUD);
          }}
          className={LINK_CLASS}
        >
          <Cloud className="size-3.5" /> Meko Cloud
        </a>
      </div>
      <div className="space-y-2 border-t pt-3">
        {hasAccount(auth) ? (
          <>
            <p className="text-sm font-medium">Account</p>
            <p className="text-xs text-muted-foreground">
              {auth.username
                ? `${auth.signed_in ? "Signed in as" : "The login is"} ${auth.username}. Change its password, turn two-step sign-in on or off, or sign out.`
                : "No login yet: run meko setup_link on the computer Meko runs on to create one."}
            </p>
            <a
              href={href(ACCOUNT)}
              onClick={(e) => {
                e.preventDefault();
                onClose();
                navigate(ACCOUNT);
              }}
              className={LINK_CLASS}
            >
              <UserRound className="size-3.5" /> Account settings
            </a>
          </>
        ) : (
          <LocalAccessNote />
        )}
      </div>
    </Dialog>
  );
}
