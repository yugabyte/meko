// Two-phase dialog for minting a Meko personal access token.
//
//   Phase 1 — form: mandatory name + Generate button.
//   Phase 2 — reveal: plaintext token shown exactly once with Copy + Download.
//             Dismissal wipes the plaintext from React state and triggers
//             the parent's onCreated() so the list refetches.
//
// The two phases are colocated because the plaintext lives in transient
// state we never want to lift up — keeping it local here means nothing
// outside this component ever holds a reference to it.

import { useCallback, useEffect, useRef, useState } from "react";
import { AlertCircle, Check, Copy, Download, KeyRound, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { copyToClipboard } from "@/lib/clipboard";
import {
  createMekoToken,
  getApiErrorMessage,
  type CreateMekoTokenResponse,
} from "@/lib/mekoApi";
import {
  MEKO_TOKEN_DEFAULT_TTL_DAYS,
  MEKO_TOKEN_NAME_MAX_LENGTH,
  MEKO_TOKEN_NAME_MIN_LENGTH,
  mekoTokenDownloadFilename,
} from "@/lib/mekoTokens";

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Fired after a successful create so the caller can refetch the list. */
  onCreated: () => void;
};

export function MekoTokenCreateDialog({ open, onOpenChange, onCreated }: Props) {
  const [name, setName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** Set after a successful create. Holds the plaintext token + record
   *  reference — wiped on dialog close so nothing outside this component
   *  ever has a long-lived handle on the secret. */
  const [revealed, setRevealed] = useState<CreateMekoTokenResponse | null>(null);
  /** `copy` | `download` | null. Drives the small "Saved" check next to
   *  the button the user used, so they get explicit feedback. */
  const [savedVia, setSavedVia] = useState<"copy" | "download" | null>(null);
  const nameInputRef = useRef<HTMLInputElement | null>(null);

  // Reset every piece of local state when the dialog closes. We do this in
  // an effect (not a ref or a layout-effect) so React always sees the wipe
  // *after* the closing transition — avoids a flash of empty reveal state.
  useEffect(() => {
    if (open) {
      // Focus the name field on open; cheap UX win, also confirms to the
      // user that typing immediately fills the form.
      window.setTimeout(() => nameInputRef.current?.focus(), 0);
      return;
    }
    setName("");
    setSubmitting(false);
    setError(null);
    setRevealed(null);
    setSavedVia(null);
  }, [open]);

  const trimmedName = name.trim();
  const nameValid =
    trimmedName.length >= MEKO_TOKEN_NAME_MIN_LENGTH &&
    trimmedName.length <= MEKO_TOKEN_NAME_MAX_LENGTH;

  const handleSubmit = useCallback(async () => {
    if (!nameValid || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const resp = await createMekoToken({
        name: trimmedName,
        ttl_days: MEKO_TOKEN_DEFAULT_TTL_DAYS,
      });
      setRevealed(resp);
      // Refetch the list immediately so the new row appears behind the
      // reveal dialog (no spinner gap once the user closes it).
      onCreated();
    } catch (err) {
      setError(getApiErrorMessage(err, "Couldn't create token."));
    } finally {
      setSubmitting(false);
    }
  }, [nameValid, submitting, trimmedName, onCreated]);

  const handleCopy = useCallback(async () => {
    if (!revealed) return;
    const ok = await copyToClipboard(revealed.token);
    if (!ok) return;
    setSavedVia("copy");
  }, [revealed]);

  const handleDownload = useCallback(() => {
    if (!revealed) return;
    const blob = new Blob([revealed.token], { type: "text/plain;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = mekoTokenDownloadFilename(revealed.token_record.name);
    document.body.appendChild(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(url);
    setSavedVia("download");
  }, [revealed]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-[#1C2333] border-border text-foreground">
        <DialogHeader>
          <div className="flex items-center gap-2">
            <KeyRound className="h-5 w-5 shrink-0 text-blue-400" aria-hidden />
            <DialogTitle className="text-white">
              {revealed ? "API key created" : "Generate API key"}
            </DialogTitle>
          </div>
          <DialogDescription className="text-muted-foreground">
            {revealed
              ? "Copy or download your token now — it won't be shown again."
              : "Used to authenticate Meko MCP clients (Claude, Cursor, …)."}
          </DialogDescription>
        </DialogHeader>

        {revealed ? (
          <RevealPhase
            token={revealed.token}
            tokenName={revealed.token_record.name}
            savedVia={savedVia}
            onCopy={handleCopy}
            onDownload={handleDownload}
            onClose={() => onOpenChange(false)}
          />
        ) : (
          <FormPhase
            name={name}
            onNameChange={setName}
            error={error}
            submitting={submitting}
            canSubmit={nameValid && !submitting}
            onSubmit={handleSubmit}
            onCancel={() => onOpenChange(false)}
            nameInputRef={nameInputRef}
          />
        )}
      </DialogContent>
    </Dialog>
  );
}

function FormPhase({
  name,
  onNameChange,
  error,
  submitting,
  canSubmit,
  onSubmit,
  onCancel,
  nameInputRef,
}: {
  name: string;
  onNameChange: (next: string) => void;
  error: string | null;
  submitting: boolean;
  canSubmit: boolean;
  onSubmit: () => void;
  onCancel: () => void;
  nameInputRef: React.RefObject<HTMLInputElement | null>;
}) {
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        onSubmit();
      }}
      className="space-y-4"
    >
      <div className="space-y-1.5">
        <Label htmlFor="meko-token-name" className="text-xs text-muted-foreground">
          Name
          <span className="ml-1 text-red-400" aria-hidden>
            *
          </span>
        </Label>
        <Input
          ref={nameInputRef}
          id="meko-token-name"
          value={name}
          onChange={(e) => onNameChange(e.target.value)}
          placeholder="e.g. laptop / claude-desktop"
          maxLength={MEKO_TOKEN_NAME_MAX_LENGTH}
          autoComplete="off"
          disabled={submitting}
        />
        <p className="text-[11px] text-muted-foreground">
          Used as a label only — pick something that identifies where this token will live.
        </p>
      </div>

      {error ? (
        <div className="flex items-start gap-2 rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-200">
          <AlertCircle className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          <span>{error}</span>
        </div>
      ) : null}

      <div className="flex items-center justify-end gap-2">
        <Button type="button" variant="ghost" onClick={onCancel} disabled={submitting}>
          Cancel
        </Button>
        <Button
          type="submit"
          disabled={!canSubmit}
          className="gap-1.5 bg-blue-600 text-white hover:bg-blue-700"
        >
          {submitting ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
          ) : (
            <KeyRound className="h-3.5 w-3.5" aria-hidden />
          )}
          Generate API key
        </Button>
      </div>
    </form>
  );
}

function RevealPhase({
  token,
  tokenName,
  savedVia,
  onCopy,
  onDownload,
  onClose,
}: {
  token: string;
  tokenName: string;
  savedVia: "copy" | "download" | null;
  onCopy: () => void;
  onDownload: () => void;
  onClose: () => void;
}) {
  const saved = savedVia !== null;
  return (
    <div className="space-y-4">
      <div className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-200">
        <p>
          <span className="font-semibold">{tokenName}</span> is shown exactly once. Save it now —
          you won't be able to recover it later.
        </p>
      </div>

      <div className="rounded-md border border-border bg-[#0E1525] p-3">
        <code className="block min-w-0 break-all font-mono text-xs text-white" aria-label="API token value">
          {token}
        </code>
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="button" variant="outline" onClick={onCopy} className="gap-1.5">
          {savedVia === "copy" ? (
            <Check className="h-3.5 w-3.5 text-green-400" aria-hidden />
          ) : (
            <Copy className="h-3.5 w-3.5" aria-hidden />
          )}
          {savedVia === "copy" ? "Copied" : "Copy"}
        </Button>
        <Button type="button" variant="outline" onClick={onDownload} className="gap-1.5">
          {savedVia === "download" ? (
            <Check className="h-3.5 w-3.5 text-green-400" aria-hidden />
          ) : (
            <Download className="h-3.5 w-3.5" aria-hidden />
          )}
          {savedVia === "download" ? "Downloaded" : "Download .txt"}
        </Button>
        <div className="ml-auto">
          <Button
            type="button"
            onClick={onClose}
            disabled={!saved}
            title={saved ? undefined : "Copy or download the token before closing."}
            className="gap-1.5 bg-blue-600 text-white hover:bg-blue-700"
          >
            Close
          </Button>
        </div>
      </div>
    </div>
  );
}
