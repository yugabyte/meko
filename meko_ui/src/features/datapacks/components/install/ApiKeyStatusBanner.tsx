// Shared "do you have an active Meko API key?" banner.
//
// Renders ONE of three things:
//   - loading / fetch error                  -> null (no flash)
//   - 0 active keys                          -> amber WarningBanner with a
//                                               "Generate API key" button
//                                               that opens the shared
//                                               MekoTokenCreateDialog
//   - 1+ active keys                         -> neutral info banner with
//                                               name + prefix chips so the
//                                               user knows which saved
//                                               plaintext maps to which key
//
// Used by:
//   - ConfigureWizardInstallStep (when editing/configuring a datapack)
//   - HomePage / "Connect" sidebar item (the workspace-level install page)
//
// Per the original design discussion: we intentionally do NOT auto-fill
// the install command with a token prefix. The prefix isn't a valid
// token, so substituting it would produce a copy-paste-broken command.
// The chips are an identity reminder only; the user pastes the full
// plaintext they saved at creation time into MEKO_TOKEN_INSTALLER_PLACEHOLDER.

import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { ExternalLink, KeyRound, Plus } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { WarningBanner } from "@/components/ui/warning-banner";
import { MekoTokenCreateDialog } from "@/features/settings/components/MekoTokenCreateDialog";
import { listMekoTokens, type MekoToken } from "@/lib/mekoApi";
import {
  MEKO_TOKEN_INSTALLER_PLACEHOLDER,
  MEKO_TOKEN_STATUS,
  mekoTokenStatus,
} from "@/lib/mekoTokens";

export function ApiKeyStatusBanner() {
  // `null` while loading or after a fetch error; otherwise the
  // resolved list of ACTIVE tokens for this user (could be empty).
  const [activeKeys, setActiveKeys] = useState<MekoToken[] | null>(null);
  // Drives the shared `MekoTokenCreateDialog`. We reuse the Settings
  // page's create dialog verbatim so the one-time plaintext reveal +
  // copy UX stays identical across the app — no duplicated flow.
  const [createOpen, setCreateOpen] = useState(false);

  const load = useCallback(async (signal?: AbortSignal) => {
    try {
      const tokens = await listMekoTokens({ signal });
      if (signal?.aborted) return;
      const active = (tokens ?? []).filter(
        (t) => mekoTokenStatus(t) === MEKO_TOKEN_STATUS.active,
      );
      setActiveKeys(active);
    } catch {
      // Stay silent on error: the rest of the page still works.
      if (!signal?.aborted) setActiveKeys(null);
    }
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    void load(ac.signal);
    return () => ac.abort();
  }, [load]);

  // IMPORTANT: the dialog is rendered at the top level — NOT inside
  // the `activeKeys.length === 0` branch — so it stays mounted across
  // the "zero keys → one key" transition that `onCreated` triggers.
  // If we put it inside that branch, the dialog would unmount the
  // instant `load()` resolves with the new key, eating the reveal
  // phase (plaintext is shown exactly once, the user would never get
  // to copy / download it).
  //
  // While `activeKeys === null` (initial load / fetch error) we render
  // the dialog but no banner — the dialog is harmless when no entry
  // point opens it.
  return (
    <>
      {activeKeys === null ? null : activeKeys.length === 0 ? (
        <WarningBanner>
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="min-w-0 flex-1">
              You don't have an active API key yet.{" "}
              <Link
                to={ROUTES.settings}
                // Was `text-amber-50` — near-white that disappeared on the
                // light-mode warning banner's pale amber background. The
                // banner sits over `bg-amber-500/10`, so we need a darker
                // amber in light mode for contrast and the lighter shade
                // only in dark mode where the band reads as deep amber.
                className="inline-flex items-center gap-0.5 font-medium text-amber-800 dark:text-amber-100 underline-offset-2 hover:underline hover:text-amber-900 dark:hover:text-amber-50"
              >
                Manage API keys in Settings → Security
                <ExternalLink className="h-3 w-3" aria-hidden />
              </Link>{" "}
              before installing the Meko MCP server — the install command below needs the API key.
            </p>
            <Button
              type="button"
              size="sm"
              variant="outline"
              // Hover token bumped to a darker amber in light mode (was
              // text-amber-50 which vanished against the pale amber band).
              className="h-8 shrink-0 gap-1.5 border-amber-500/40 bg-amber-500/15 text-amber-800 dark:text-amber-100 hover:bg-amber-500/25 hover:text-amber-900 dark:hover:text-amber-50"
              onClick={() => setCreateOpen(true)}
            >
              <Plus className="h-3.5 w-3.5" aria-hidden />
              Generate API key
            </Button>
          </div>
        </WarningBanner>
      ) : (
        <ActiveApiKeysHint keys={activeKeys} />
      )}
      <MekoTokenCreateDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={() => void load()}
      />
    </>
  );
}

/** Neutral info card. Lists the user's active keys by `name · prefix…`
 *  so they can identify which saved plaintext maps to which chip when
 *  they're filling in the install command. We deliberately don't
 *  auto-fill — we don't have the plaintext, only the prefix. */
function ActiveApiKeysHint({ keys }: { keys: MekoToken[] }) {
  const count = keys.length;
  return (
    <div
      role="status"
      className="flex flex-wrap items-start gap-3 rounded-md border border-blue-500/30 bg-blue-500/[0.06] px-4 py-3 text-sm leading-relaxed text-foreground/90"
    >
      <KeyRound className="mt-0.5 h-4 w-4 shrink-0 text-blue-700 dark:text-blue-300" aria-hidden />
      <div className="min-w-0 flex-1 space-y-2">
        <p>
          You have <span className="font-medium text-foreground">{count}</span> active API key
          {count === 1 ? "" : "s"}. Paste the full key you saved at creation time into the install
          command below — {" "}
          <code className="rounded bg-background/60 px-1 py-0.5 text-[12px]">
            {MEKO_TOKEN_INSTALLER_PLACEHOLDER}
          </code>{" "}
        </p>
        <ul className="flex flex-wrap gap-1.5">
          {keys.map((k) => (
            <li key={k.id}>
              <Badge
                variant="outline"
                className="gap-1.5 border-blue-500/35 bg-blue-500/10 text-blue-700 dark:text-blue-100"
                title={`API key: ${k.name} (prefix ${k.token_prefix}…)`}
              >
                <KeyRound className="h-3 w-3 text-blue-700 dark:text-blue-300" aria-hidden />
                <span className="font-medium">{k.name}</span>
                <code className="font-mono text-blue-700 dark:text-blue-200/85">{k.token_prefix}…</code>
              </Badge>
            </li>
          ))}
        </ul>
        <p className="text-xs text-muted-foreground">
          Lost the plaintext?{" "}
          <Link
            to={ROUTES.settings}
            className="inline-flex items-center gap-0.5 font-medium text-blue-700 dark:text-blue-300 hover:text-blue-700 dark:text-blue-200"
          >
            Manage in Settings → Security
            <ExternalLink className="h-3 w-3" aria-hidden />
          </Link>{" "}
          (revoke and generate a new one).
        </p>
      </div>
    </div>
  );
}
