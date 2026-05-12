// Modal for the signed-in change-password flow. Three fields:
//
//   1. Old password    — current Cognito password (the BE verifies via
//                        ChangePassword's PreviousPassword arg).
//   2. New password    — must clear the same min-length bar the
//                        ForgotPassword flow uses (kept consistent so
//                        users don't get conflicting messages between
//                        the two surfaces).
//   3. Confirm         — must match new password before submit.
//
// Validation runs client-side first; on submit we POST to the BE. Errors
// (wrong old password, weak new password per Cognito policy, etc.)
// surface inline rather than via toast so they're attached to the
// field they belong to.

import { useEffect, useState, type FormEvent } from "react";
import { Eye, EyeOff, Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import { changePassword, getApiErrorMessage } from "@/lib/mekoApi";

/** Lower bound matches `ForgotPasswordPage.MIN_NEW_PASSWORD_LEN` so the
 *  two flows stay consistent. Cognito's actual policy may be stricter;
 *  any additional failure surfaces verbatim from the BE response. */
const MIN_NEW_PASSWORD_LEN = 7;

function validate(
  oldPassword: string,
  newPassword: string,
  confirmPassword: string,
): string | null {
  if (!oldPassword) return "Enter your current password.";
  if (newPassword.length < MIN_NEW_PASSWORD_LEN) {
    return `New password must be at least ${MIN_NEW_PASSWORD_LEN} characters.`;
  }
  if (newPassword === oldPassword) {
    return "New password must be different from the current one.";
  }
  if (newPassword !== confirmPassword) {
    return "New password and confirmation do not match.";
  }
  return null;
}

type Props = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
};

export function UpdatePasswordDialog({ open, onOpenChange }: Props) {
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [showPasswords, setShowPasswords] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Reset every time the dialog opens so a previous attempt's state
  // doesn't carry over to a new session.
  useEffect(() => {
    if (open) {
      setOldPassword("");
      setNewPassword("");
      setConfirmPassword("");
      setShowPasswords(false);
      setError(null);
    }
  }, [open]);

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    if (submitting) return;
    const validationErr = validate(oldPassword, newPassword, confirmPassword);
    if (validationErr) {
      setError(validationErr);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await changePassword({
        old_password: oldPassword,
        new_password: newPassword,
        confirm_password: confirmPassword,
      });
      toast({ title: "Password updated" });
      onOpenChange(false);
    } catch (err) {
      // BE returns Cognito error text in `detail` (wrong password,
      // policy violation, etc.) — surface verbatim instead of a generic
      // "couldn't update" so the user knows what to fix.
      setError(getApiErrorMessage(err, "Couldn't update password."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Update password</DialogTitle>
        </DialogHeader>

        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-1.5">
            <Label htmlFor="settings-old-password">Current password</Label>
            <Input
              id="settings-old-password"
              type={showPasswords ? "text" : "password"}
              autoComplete="current-password"
              value={oldPassword}
              onChange={(e) => setOldPassword(e.target.value)}
              disabled={submitting}
              required
              autoFocus
            />
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="settings-new-password">New password</Label>
            <Input
              id="settings-new-password"
              type={showPasswords ? "text" : "password"}
              autoComplete="new-password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              disabled={submitting}
              required
            />
            <p className="text-[11px] text-muted-foreground">
              At least {MIN_NEW_PASSWORD_LEN} characters.
            </p>
          </div>

          <div className="space-y-1.5">
            <Label htmlFor="settings-confirm-password">
              Confirm new password
            </Label>
            <Input
              id="settings-confirm-password"
              type={showPasswords ? "text" : "password"}
              autoComplete="new-password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              disabled={submitting}
              required
            />
          </div>

          <div className="flex items-center justify-between text-xs">
            <button
              type="button"
              onClick={() => setShowPasswords((v) => !v)}
              className="inline-flex items-center gap-1.5 text-muted-foreground transition-colors hover:text-foreground"
            >
              {showPasswords ? (
                <>
                  <EyeOff className="h-3.5 w-3.5" aria-hidden />
                  Hide passwords
                </>
              ) : (
                <>
                  <Eye className="h-3.5 w-3.5" aria-hidden />
                  Show passwords
                </>
              )}
            </button>
          </div>

          {error ? (
            <p
              className={cn(
                "rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300",
              )}
            >
              {error}
            </p>
          ) : null}

          <DialogFooter className="gap-2 sm:gap-0">
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={submitting}>
              {submitting ? (
                <>
                  <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
                  Updating…
                </>
              ) : (
                "Update password"
              )}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
