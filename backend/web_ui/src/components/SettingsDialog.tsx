// Meko's settings: whether usage stats go to YugabyteDB, saved as soon as it's changed. Turned off
// by config.toml or the environment, the checkbox is off and can't be turned on here.

import { useEffect, useState } from "react";
import { api } from "../lib/api";
import type { UsageStats } from "../lib/types";
import { Dialog } from "./Dialog";
import { Button, ErrorNote, Spinner } from "./ui";

function note(u: UsageStats): string | null {
  if (u.forced_off) return `Turned off by ${u.by}. Remove that setting and restart Meko to turn them on here.`;
  if (u.enabled && !u.sends) return "A build from source sends none.";
  return null;
}

export function SettingsDialog({ onClose }: { onClose: () => void }) {
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
    </Dialog>
  );
}
