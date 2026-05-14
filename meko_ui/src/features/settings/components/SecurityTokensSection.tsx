// Settings → Security → API tokens.
//
// Owns the list/create/delete lifecycle for Meko personal access tokens
// (MCP PATs). Plaintext only ever lives transiently inside
// `MekoTokenCreateDialog` — this section only ever sees prefixes.
//
// The row component is memoized so a single delete doesn't re-render the
// other rows (matters once a user is near the 10-token cap).

import { memo, useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertCircle,
  KeyRound,
  Loader2,
  Plus,
  RefreshCw,
  Trash2,
} from "lucide-react";
import { Button } from "@/components/ui/button";
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
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import {
  getApiErrorMessage,
  listMekoTokens,
  revokeMekoToken,
  type MekoToken,
} from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import {
  MEKO_TOKEN_MAX_COUNT,
  MEKO_TOKEN_STATUS,
  mekoTokenStatus,
  type MekoTokenStatus,
} from "@/lib/mekoTokens";
import { MekoTokenCreateDialog } from "./MekoTokenCreateDialog";

const STATUS_TONE: Record<MekoTokenStatus, string> = {
  [MEKO_TOKEN_STATUS.active]: "border-emerald-500/30 bg-emerald-500/10 text-emerald-200",
  [MEKO_TOKEN_STATUS.expired]: "border-amber-500/30 bg-amber-500/10 text-amber-200",
  [MEKO_TOKEN_STATUS.revoked]: "border-red-500/30 bg-red-500/10 text-red-200",
};

const STATUS_LABEL: Record<MekoTokenStatus, string> = {
  [MEKO_TOKEN_STATUS.active]: "Active",
  [MEKO_TOKEN_STATUS.expired]: "Expired",
  [MEKO_TOKEN_STATUS.revoked]: "Revoked",
};

export function SecurityTokensSection() {
  const [tokens, setTokens] = useState<MekoToken[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);
  const [pendingRevokeId, setPendingRevokeId] = useState<string | null>(null);
  const [revokingId, setRevokingId] = useState<string | null>(null);

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    setError(null);
    try {
      const list = await listMekoTokens({ signal });
      setTokens(list ?? []);
    } catch (err) {
      if ((err as { name?: string })?.name === "CanceledError") return;
      setError(getApiErrorMessage(err, "Failed to load tokens."));
      setTokens([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    void load(ac.signal);
    return () => ac.abort();
  }, [load]);

  // Stable callback so each TokenRow can compare props by reference and
  // skip re-rendering when nothing changes.
  const requestRevoke = useCallback((tokenId: string) => {
    setPendingRevokeId(tokenId);
  }, []);

  const confirmRevoke = useCallback(async () => {
    if (!pendingRevokeId) return;
    const tokenId = pendingRevokeId;
    setRevokingId(tokenId);
    try {
      await revokeMekoToken(tokenId);
      toast({ title: "Token revoked" });
      setPendingRevokeId(null);
      await load();
    } catch (err) {
      toast({
        title: "Couldn't revoke token",
        description: getApiErrorMessage(err, "Revoke failed."),
      });
    } finally {
      setRevokingId(null);
    }
  }, [pendingRevokeId, load]);

  const activeCount = useMemo(
    () => tokens.filter((t) => mekoTokenStatus(t) === MEKO_TOKEN_STATUS.active).length,
    [tokens],
  );
  const atCap = activeCount >= MEKO_TOKEN_MAX_COUNT;
  const pendingToken = useMemo(
    () => tokens.find((t) => t.id === pendingRevokeId) ?? null,
    [tokens, pendingRevokeId],
  );

  return (
    <TooltipProvider delayDuration={200}>
      <section className="rounded-xl border border-border bg-[#0F1626] p-5">
        <header className="mb-4 flex items-start justify-between gap-3">
          <div>
            <h2 className="text-sm font-semibold text-white">API tokens</h2>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Personal access tokens for connecting MCP clients (Claude, Cursor, …) to your datapacks.{" "}
              <span className="text-foreground/80">
                {activeCount} of {MEKO_TOKEN_MAX_COUNT} active
              </span>
              .
            </p>
          </div>
          <div className="flex shrink-0 items-center gap-2">
            <Button
              type="button"
              variant="ghost"
              size="sm"
              onClick={() => void load()}
              disabled={loading}
              className="h-8 gap-1.5 text-muted-foreground hover:text-foreground"
              aria-label="Refresh tokens"
            >
              {loading ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : (
                <RefreshCw className="h-3.5 w-3.5" aria-hidden />
              )}
            </Button>
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="inline-flex">
                  <Button
                    type="button"
                    variant="outline"
                    size="sm"
                    className="h-8 gap-1.5"
                    onClick={() => setCreateOpen(true)}
                    disabled={atCap}
                  >
                    <Plus className="h-3.5 w-3.5" aria-hidden />
                    Generate token
                  </Button>
                </span>
              </TooltipTrigger>
              {atCap ? (
                <TooltipContent side="top" className="max-w-[260px] text-center">
                  You've reached the {MEKO_TOKEN_MAX_COUNT}-token limit. Revoke an unused token to generate a new one.
                </TooltipContent>
              ) : null}
            </Tooltip>
          </div>
        </header>

        {error ? (
          <div className="mb-3 flex items-start gap-2 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-200">
            <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            <span>{error}</span>
          </div>
        ) : null}

        {loading && tokens.length === 0 ? (
          <p className="text-xs text-muted-foreground">Loading tokens…</p>
        ) : !loading && tokens.length === 0 ? (
          <EmptyState onGenerate={() => setCreateOpen(true)} />
        ) : (
          <ul className="space-y-2">
            {tokens.map((token) => (
              <TokenRow
                key={token.id}
                token={token}
                onRevoke={requestRevoke}
                isRevoking={revokingId === token.id}
              />
            ))}
          </ul>
        )}
      </section>

      <MekoTokenCreateDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={() => void load()}
      />

      <AlertDialog
        open={pendingRevokeId !== null}
        onOpenChange={(next) => {
          if (!next) setPendingRevokeId(null);
        }}
      >
        <AlertDialogContent className="bg-[#1C2333] border-border">
          <AlertDialogHeader>
            <AlertDialogTitle className="text-white">Revoke this token?</AlertDialogTitle>
            <AlertDialogDescription className="text-muted-foreground">
              {pendingToken ? (
                <>
                  Any MCP client using <span className="text-white">{pendingToken.name}</span>{" "}
                  (<span className="font-mono text-xs">{pendingToken.token_prefix}…</span>) will stop
                  working immediately. This cannot be undone.
                </>
              ) : (
                <>This action cannot be undone.</>
              )}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={revokingId !== null}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={(e) => {
                // AlertDialogAction closes the dialog by default; we need it
                // to stay open while the request is in flight so we can show
                // the spinner.
                e.preventDefault();
                void confirmRevoke();
              }}
              disabled={revokingId !== null}
              className="bg-red-600 text-white hover:bg-red-700"
            >
              {revokingId !== null ? (
                <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : null}
              Revoke
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </TooltipProvider>
  );
}

function EmptyState({ onGenerate }: { onGenerate: () => void }) {
  return (
    <div className="rounded-lg border border-dashed border-border bg-[#0E1525]/40 px-4 py-6 text-center">
      <KeyRound className="mx-auto mb-2 h-5 w-5 text-muted-foreground" aria-hidden />
      <p className="text-xs text-muted-foreground">
        No API tokens yet. Generate one to connect MCP clients to your datapacks.
      </p>
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="mt-3 gap-1.5"
        onClick={onGenerate}
      >
        <Plus className="h-3.5 w-3.5" aria-hidden />
        Generate token
      </Button>
    </div>
  );
}

type TokenRowProps = {
  token: MekoToken;
  onRevoke: (tokenId: string) => void;
  isRevoking: boolean;
};

const TokenRow = memo(function TokenRow({ token, onRevoke, isRevoking }: TokenRowProps) {
  const status = mekoTokenStatus(token);
  const canRevoke = status === MEKO_TOKEN_STATUS.active;
  const lastUsed = token.last_used_at
    ? `Last used ${formatDatapackCreatedAtUtc(token.last_used_at)}`
    : "Never used";
  // Active → "Expires <date>" (when it WILL stop working).
  // Expired → "Expired <date>" (the moment it stopped working — same field).
  // Revoked → "Revoked <date>" (revocation supersedes expiry; show revoked_at).
  const expiryLabel =
    status === MEKO_TOKEN_STATUS.revoked && token.revoked_at
      ? `Revoked ${formatDatapackCreatedAtUtc(token.revoked_at)}`
      : status === MEKO_TOKEN_STATUS.expired
        ? `Expired ${formatDatapackCreatedAtUtc(token.expires_at)}`
        : `Expires ${formatDatapackCreatedAtUtc(token.expires_at)}`;
  return (
    <li className="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-3">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="truncate text-sm font-medium text-white">{token.name}</span>
          <span
            className={cn(
              "inline-flex items-center rounded-full border px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide",
              STATUS_TONE[status],
            )}
          >
            {STATUS_LABEL[status]}
          </span>
        </div>
        <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-[11px] text-muted-foreground">
          <code className="font-mono text-foreground/80">{token.token_prefix}…</code>
          <span>Created {formatDatapackCreatedAtUtc(token.created_at)}</span>
          <span>{expiryLabel}</span>
          <span>{lastUsed}</span>
        </div>
      </div>
      <Button
        type="button"
        variant="ghost"
        size="sm"
        className="h-8 gap-1.5 text-muted-foreground hover:text-red-300"
        onClick={() => onRevoke(token.id)}
        disabled={!canRevoke || isRevoking}
        aria-label={`Revoke ${token.name}`}
        title={canRevoke ? "Revoke" : `Token is ${STATUS_LABEL[status].toLowerCase()}`}
      >
        {isRevoking ? (
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
        ) : (
          <Trash2 className="h-3.5 w-3.5" aria-hidden />
        )}
        Revoke
      </Button>
    </li>
  );
});
