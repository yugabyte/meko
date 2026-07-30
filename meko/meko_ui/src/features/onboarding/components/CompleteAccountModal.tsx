import { type KeyboardEvent, useState } from "react";
import { Loader2 } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";

import {
  getApiErrorMessage,
  resendEmailChangeCode,
  startEmailChange,
  updateProfile,
  verifyEmailChange,
} from "@/lib/api/mekoApi";
import { CURRENT_USER_QUERY_KEY } from "@/hooks/useCurrentUser";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

type Props = {
  /** Server-driven flags for what's still missing. At least one is true when
   *  this modal renders (the parent only mounts us when something is needed). */
  needsName: boolean;
  needsEmail: boolean;
  /** Provider-supplied name parts (best-effort) to pre-fill, when needsName. */
  initialFirstName: string;
  initialLastName: string;
  /** Called only after EVERY required piece is saved/verified, so the parent
   *  can stop rendering us. The modal can't be dismissed any other way (no
   *  skip, no outside-click, no escape). */
  onDone: () => void;
};

// Single mandatory modal that completes a social signup's missing profile data.
// It dynamically handles any combination of:
//   - missing name  (the IdP's last name is optional and may be absent), and
//   - missing/unverified email (LinkedIn's email claim is optional, so the
//     account carries a synthesized placeholder until verified).
// Both pieces are collected on one "details" step; if an email was collected we
// then require an OTP (emailed by Cognito) before finishing. Driven purely by
// server state, so abandoning the tab and returning re-prompts for whatever is
// still incomplete. Email/password signups have both pieces and never see this.
export default function CompleteAccountModal({
  needsName,
  needsEmail,
  initialFirstName,
  initialLastName,
  onDone,
}: Props) {
  // "details" → collect name + email (sends OTP); "otp" → confirm the code.
  const [step, setStep] = useState<"details" | "otp">("details");
  const [firstName, setFirstName] = useState(initialFirstName);
  const [lastName, setLastName] = useState(initialLastName);
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  // Name is saved before the email OTP round-trip; track it so an email error
  // (e.g. address taken) doesn't re-save the name on retry.
  const [nameSaved, setNameSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [resending, setResending] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const queryClient = useQueryClient();

  // Refresh the cached /auth/me before handing control back so anything reading
  // it (notably the navbar's "Signed in as …") swaps the synthesized placeholder
  // for the just-verified real email instead of staying stale.
  const finish = () => {
    void queryClient.invalidateQueries({ queryKey: [CURRENT_USER_QUERY_KEY] });
    onDone();
  };

  const trimmedFirst = firstName.trim();
  const trimmedLast = lastName.trim();
  const trimmedEmail = email.trim();
  const trimmedCode = code.trim();

  const nameOk = !needsName || (trimmedFirst !== "" && trimmedLast !== "");
  // Light client-side shape check; the BE validates authoritatively.
  const emailOk = !needsEmail || /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmedEmail);
  const canContinue = nameOk && emailOk && !busy;
  // Cognito codes are exactly 6 digits. We don't gate the button on that length
  // (that would leak the expected size); any non-empty entry can be submitted
  // and a wrong length is rejected with the same generic "wrong code" error.
  const CODE_LENGTH = 6;
  const canVerify = trimmedCode.length > 0 && !busy;

  // Keep the OTP field digits-only (handles paste too, since onChange fires for
  // pastes). Capped at a generous length so a wrong-size entry is still possible
  // — we deliberately don't pin the field to the real code length.
  const MAX_CODE_INPUT = 10;
  const handleCodeChange = (raw: string) => {
    setCode(raw.replace(/\D/g, "").slice(0, MAX_CODE_INPUT));
  };

  const handleContinue = async () => {
    if (!canContinue) return;
    setError(null);
    setNotice(null);
    setBusy(true);
    try {
      if (needsName && !nameSaved) {
        await updateProfile(trimmedFirst, trimmedLast);
        setNameSaved(true);
      }
      if (needsEmail) {
        // Writes the email to Cognito, which emails the OTP, and records it
        // locally as unverified. Advance to the code step.
        await startEmailChange(trimmedEmail);
        setStep("otp");
        setNotice(`We sent a 6-digit code to ${trimmedEmail}.`);
        setBusy(false);
        return;
      }
      // Name-only flow: nothing left to verify.
      finish();
    } catch (err) {
      setError(getApiErrorMessage(err, "Couldn't save your details. Please try again."));
      setBusy(false);
    }
  };

  const handleVerify = async () => {
    if (!canVerify) return;
    setError(null);
    setNotice(null);
    // Wrong length is just a wrong code — same generic message, no size hint,
    // and no need to round-trip the server for an obviously invalid entry.
    if (trimmedCode.length !== CODE_LENGTH) {
      setError("That code didn't work. Please try again.");
      return;
    }
    setBusy(true);
    try {
      await verifyEmailChange(trimmedCode);
      finish();
    } catch (err) {
      setError(getApiErrorMessage(err, "That code didn't work. Please try again."));
      setBusy(false);
    }
    // On success we leave `busy` true: the parent unmounts us.
  };

  const handleResend = async () => {
    if (resending || busy) return;
    setError(null);
    setNotice(null);
    setResending(true);
    try {
      await resendEmailChangeCode();
      setNotice(`We sent a new code to ${trimmedEmail}.`);
    } catch (err) {
      setError(getApiErrorMessage(err, "Couldn't resend the code. Please try again."));
    } finally {
      setResending(false);
    }
  };

  // Back to the details step to fix a typo'd address before verifying.
  const backToDetails = () => {
    if (busy) return;
    setStep("details");
    setCode("");
    setError(null);
    setNotice(null);
  };

  const submitOnEnter =
    (action: () => void, enabled: boolean) => (e: KeyboardEvent<HTMLInputElement>) => {
      if (e.key === "Enter" && enabled) {
        e.preventDefault();
        action();
      }
    };

  // Title/description adapt to what's being collected.
  const detailsTitle =
    needsName && needsEmail
      ? "Finish setting up your account"
      : needsEmail
        ? "Confirm your email"
        : "Confirm your name";
  const detailsDescription =
    needsName && needsEmail
      ? "Add your name and an email so we can finish setting up your account — we'll send a code to confirm the email."
      : needsEmail
        ? "Your sign-in provider didn't share an email, so add one to finish setting up your account — we'll send a code to confirm it."
        : "Add your first and last name to continue — this is the name we'll use for your account.";

  const detailsCta = needsEmail ? "Send code" : "Looks good";
  const detailsBusyLabel = needsEmail ? "Sending…" : "Saving…";

  return (
    <Dialog open>
      <DialogContent
        // `[&>button]:hidden` hides DialogContent's built-in close (X).
        className="max-w-md gap-0 overflow-hidden p-0 [&>button]:hidden"
        // Mandatory step: block every dismissal path so the user can only leave
        // by completing it.
        onInteractOutside={(e) => e.preventDefault()}
        onEscapeKeyDown={(e) => e.preventDefault()}
      >
        <div className="space-y-5 p-6">
          <DialogHeader className="space-y-1.5">
            <DialogTitle>
              {step === "details" ? detailsTitle : "Enter the verification code"}
            </DialogTitle>
            <DialogDescription>
              {step === "details"
                ? detailsDescription
                : "Check your inbox for the 6-digit code and enter it below to verify your email."}
            </DialogDescription>
          </DialogHeader>

          {step === "details" ? (
            <div className="space-y-4">
              {needsName ? (
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                  <div className="space-y-1.5">
                    <Label htmlFor="complete-first-name">First name</Label>
                    <Input
                      id="complete-first-name"
                      value={firstName}
                      onChange={(e) => setFirstName(e.target.value)}
                      onKeyDown={submitOnEnter(() => void handleContinue(), canContinue)}
                      autoComplete="given-name"
                      disabled={busy}
                      autoFocus
                      className="border-border bg-input text-foreground"
                    />
                  </div>
                  <div className="space-y-1.5">
                    <Label htmlFor="complete-last-name">Last name</Label>
                    <Input
                      id="complete-last-name"
                      value={lastName}
                      onChange={(e) => setLastName(e.target.value)}
                      onKeyDown={submitOnEnter(() => void handleContinue(), canContinue)}
                      autoComplete="family-name"
                      disabled={busy}
                      className="border-border bg-input text-foreground"
                    />
                  </div>
                </div>
              ) : null}

              {needsEmail ? (
                <div className="space-y-1.5">
                  <Label htmlFor="complete-email">Email address</Label>
                  <Input
                    id="complete-email"
                    type="email"
                    inputMode="email"
                    value={email}
                    onChange={(e) => setEmail(e.target.value)}
                    onKeyDown={submitOnEnter(() => void handleContinue(), canContinue)}
                    autoComplete="email"
                    disabled={busy}
                    autoFocus={!needsName}
                    placeholder="you@example.com"
                    className="border-border bg-input text-foreground"
                  />
                </div>
              ) : null}
            </div>
          ) : (
            <div className="space-y-1.5">
              <Label htmlFor="complete-code">Verification code</Label>
              <Input
                id="complete-code"
                inputMode="numeric"
                pattern="\d*"
                maxLength={MAX_CODE_INPUT}
                autoComplete="one-time-code"
                value={code}
                onChange={(e) => handleCodeChange(e.target.value)}
                onKeyDown={submitOnEnter(() => void handleVerify(), canVerify)}
                disabled={busy}
                autoFocus
                placeholder="123456"
                className="border-border bg-input tracking-widest text-foreground"
              />
              <div className="flex items-center justify-between pt-1 text-xs">
                <button
                  type="button"
                  onClick={backToDetails}
                  disabled={busy}
                  className="text-muted-foreground transition hover:text-foreground disabled:opacity-50"
                >
                  Use a different email
                </button>
                <button
                  type="button"
                  onClick={() => void handleResend()}
                  disabled={resending || busy}
                  className="text-muted-foreground transition hover:text-foreground disabled:opacity-50"
                >
                  {resending ? "Sending…" : "Resend code"}
                </button>
              </div>
            </div>
          )}

          {notice ? <p className="text-sm text-muted-foreground">{notice}</p> : null}
          {error ? <p className="text-sm text-destructive">{error}</p> : null}
        </div>

        <DialogFooter className="border-t border-border bg-muted/30 px-6 py-4">
          {step === "details" ? (
            <Button type="button" onClick={() => void handleContinue()} disabled={!canContinue}>
              {busy ? (
                <>
                  <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
                  {detailsBusyLabel}
                </>
              ) : (
                detailsCta
              )}
            </Button>
          ) : (
            <Button type="button" onClick={() => void handleVerify()} disabled={!canVerify}>
              {busy ? (
                <>
                  <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
                  Verifying…
                </>
              ) : (
                "Verify email"
              )}
            </Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
