// "API token" field for the Connect-to-Datapack dialog.
//
// Renders one of four states from the same `GET /me/meko-tokens` fetch:
//   - loading: small spinner row
//   - error:   inline red banner
//   - empty:   dashed empty-state + "Generate in Settings →" CTA
//   - list:    name + prefix per active token with a copy-prefix button
//
// Extracted out of DatapackConnectDialog so the dialog's render stays
// focused on layout — the four token states would otherwise double the
// dialog's size.

import { Link } from "react-router-dom";
import { AlertCircle, Check, Copy, KeyRound, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { ROUTES } from "@/app/routes/paths";
import type { MekoToken } from "@/lib/mekoApi";

type Props = {
  loading: boolean;
  error: string | null;
  /** Active tokens only (caller filters via `filterActiveMekoTokens`). */
  tokens: MekoToken[];
  /** All tokens including expired/revoked. Drives empty-state copy so
   *  users with tokens-that-no-longer-work see a more helpful hint. */
  totalCount: number;
  /** Shared with the parent dialog so a single `copiedField` state can
   *  drive copy-state for every field (datapack name, URL, each token). */
  copiedField: string | null;
  onCopy: (text: string, field: string) => void;
  /** Called when the user clicks the "Generate in Settings" CTA from
   *  either the empty state or the manage-tokens link. Parent should
   *  close the dialog and navigate to /settings. */
  onOpenSettings: () => void;
};

export function ConnectApiTokensField({
  loading,
  error,
  tokens,
  totalCount,
  copiedField,
  onCopy,
  onOpenSettings,
}: Props) {
  return (
    <div className="space-y-2">
      <label className="text-sm font-medium text-muted-foreground">API key</label>
      {loading ? (
        <div className="flex items-center gap-2 rounded-md border border-border bg-card px-3 py-2 text-xs text-muted-foreground">
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
          Loading tokens…
        </div>
      ) : error ? (
        <div className="flex items-start gap-2 rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-700 dark:text-red-200">
          <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          <span>{error}</span>
        </div>
      ) : tokens.length === 0 ? (
        <NoTokensEmptyState totalCount={totalCount} onOpenSettings={onOpenSettings} />
      ) : (
        <>
          <p className="text-[11px] text-muted-foreground">
            Use the full token you saved when generating it — only the prefix is shown here.
          </p>
          <ul className="space-y-2">
            {tokens.map((t) => (
              <li
                key={t.id}
                className="flex items-center gap-3 rounded-md border border-border bg-card px-3 py-2"
              >
                <KeyRound className="h-3.5 w-3.5 shrink-0 text-blue-700 dark:text-blue-300" aria-hidden />
                <div className="min-w-0 flex-1">
                  <div className="truncate text-sm text-foreground">{t.name}</div>
                  <code className="block min-w-0 truncate font-mono text-[11px] text-muted-foreground">
                    {t.token_prefix}…
                  </code>
                </div>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon"
                  className="h-8 w-8 shrink-0 text-muted-foreground hover:text-white"
                  onClick={() => void onCopy(t.token_prefix, `token-${t.id}`)}
                  aria-label={`Copy prefix for ${t.name}`}
                  title="Copy prefix"
                >
                  {copiedField === `token-${t.id}` ? (
                    <Check className="h-4 w-4 text-green-500" />
                  ) : (
                    <Copy className="h-4 w-4" />
                  )}
                </Button>
              </li>
            ))}
          </ul>
          <p className="text-[11px] text-muted-foreground">
            Need another?{" "}
            <Link
              to={ROUTES.settings}
              onClick={onOpenSettings}
              className="font-medium text-blue-700 dark:text-blue-300 hover:text-blue-700 dark:text-blue-200"
            >
              Manage tokens in Settings →
            </Link>
          </p>
        </>
      )}
    </div>
  );
}

function NoTokensEmptyState({
  totalCount,
  onOpenSettings,
}: {
  totalCount: number;
  onOpenSettings: () => void;
}) {
  return (
    <div className="flex items-start gap-3 rounded-md border border-dashed border-border bg-input/60 px-4 py-3">
      <KeyRound className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
      <div className="min-w-0 flex-1">
        <p className="text-sm text-foreground">No active API keys.</p>
        <p className="mt-0.5 text-xs text-muted-foreground">
          {totalCount > 0
            ? "Your tokens are expired or revoked. Generate a new one in Settings."
            : "Generate an API token in Settings to authenticate your MCP clients."}
        </p>
      </div>
      <Button
        type="button"
        size="sm"
        variant="outline"
        onClick={onOpenSettings}
        className="shrink-0 gap-1.5"
      >
        Generate in Settings →
      </Button>
    </div>
  );
}
