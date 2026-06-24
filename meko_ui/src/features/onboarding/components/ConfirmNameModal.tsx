import { type KeyboardEvent, useState } from "react";
import { Loader2 } from "lucide-react";

import { getApiErrorMessage, updateProfile } from "@/lib/api/mekoApi";
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
  /** Names from the provider (best-effort), used to pre-fill the fields. */
  initialFirstName: string;
  initialLastName: string;
  /** Called only after a successful save so the parent can clear the
   *  trigger and stop rendering us. The modal can't be dismissed any other
   *  way (no skip, no outside-click, no escape). */
  onDone: () => void;
};

// Mandatory modal shown over the onboarding page when a social signup's name
// is incomplete (e.g. the provider's last name is optional and may be absent).
// Email/password and merged accounts always have both names, so they never
// see it. The user MUST fill in both parts to continue — there is no skip
// and the dialog can't be dismissed without saving.
export default function ConfirmNameModal({
  initialFirstName,
  initialLastName,
  onDone,
}: Props) {
  const [firstName, setFirstName] = useState(initialFirstName);
  const [lastName, setLastName] = useState(initialLastName);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const trimmedFirst = firstName.trim();
  const trimmedLast = lastName.trim();
  const bothFilled = trimmedFirst !== "" && trimmedLast !== "";
  const canSave = bothFilled && !saving;

  // Generic, provider-neutral copy (we don't name the IdP here).
  const description = bothFilled
    ? "Make sure your name looks right — this is the name we'll use for your account."
    : "Add your first and last name to continue — this is the name we'll use for your account.";

  const handleSave = async () => {
    if (!canSave) return;
    setError(null);
    setSaving(true);
    try {
      await updateProfile(trimmedFirst, trimmedLast);
      onDone();
    } catch (err) {
      setError(getApiErrorMessage(err, "Couldn't save your name. Please try again."));
      setSaving(false);
    }
  };

  const submitOnEnter = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" && canSave) {
      e.preventDefault();
      void handleSave();
    }
  };

  return (
    <Dialog open>
      <DialogContent
        // `[&>button]:hidden` hides DialogContent's built-in close (X), the
        // only direct-child <button>; the footer button is nested deeper and
        // is unaffected.
        className="max-w-md gap-0 overflow-hidden p-0 [&>button]:hidden"
        // Mandatory step: block every dismissal path (outside click + escape)
        // so the user can only leave by saving. The Dialog is also controlled
        // open, so it can't self-close.
        onInteractOutside={(e) => e.preventDefault()}
        onEscapeKeyDown={(e) => e.preventDefault()}
      >
        <div className="space-y-5 p-6">
          <DialogHeader className="space-y-1.5">
            <DialogTitle>Confirm your name</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <div className="space-y-1.5">
              <Label htmlFor="confirm-first-name">First name</Label>
              <Input
                id="confirm-first-name"
                value={firstName}
                onChange={(e) => setFirstName(e.target.value)}
                onKeyDown={submitOnEnter}
                autoComplete="given-name"
                disabled={saving}
                autoFocus
                className="border-border bg-input text-foreground"
              />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="confirm-last-name">Last name</Label>
              <Input
                id="confirm-last-name"
                value={lastName}
                onChange={(e) => setLastName(e.target.value)}
                onKeyDown={submitOnEnter}
                autoComplete="family-name"
                disabled={saving}
                className="border-border bg-input text-foreground"
              />
            </div>
          </div>

          {error ? <p className="text-sm text-destructive">{error}</p> : null}
        </div>

        <DialogFooter className="border-t border-border bg-muted/30 px-6 py-4">
          <Button type="button" onClick={() => void handleSave()} disabled={!canSave}>
            {saving ? (
              <>
                <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
                Saving…
              </>
            ) : (
              "Looks good"
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
