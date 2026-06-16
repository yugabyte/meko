// Card UI for inviting one person by email with a grant role; posts to inviteDatapackShare on Send invite.
// Role tiles and the dropdown stay in sync; parent receives onInviteSent to refetch members/pending lists.
import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { SHARE_INVITE_ROLES, type ShareInviteRoleId } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { parseSingleInviteEmail } from "@/features/datapacks/components/datapack-share/shareInviteEmail";
import { DATAPACK_SHARE_GRANT_CONTRIBUTOR } from "@/lib/datapackShareGrants";
import { getApiErrorMessage, inviteDatapackShare } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";

type Props = {
  datapackId: string;
  /** Email of the currently signed-in user — used to block self-invites client-side. */
  currentUserEmail?: string;
  onInviteSent: () => void | Promise<void>;
};

export function InviteByEmailSection({ datapackId, currentUserEmail, onInviteSent }: Props) {
  const [email, setEmail] = useState("");
  const [emailError, setEmailError] = useState<string | null>(null);
  const [inviteRole, setInviteRole] = useState<ShareInviteRoleId>(DATAPACK_SHARE_GRANT_CONTRIBUTOR);
  // Mirrors inviteRole so the three role tiles reflect the same selection as the dropdown.
  const [tileRole, setTileRole] = useState<ShareInviteRoleId>(DATAPACK_SHARE_GRANT_CONTRIBUTOR);
  const [submitting, setSubmitting] = useState(false);

  const syncSelectFromTiles = (id: ShareInviteRoleId) => {
    setTileRole(id);
    setInviteRole(id);
  };

  const handleSend = async () => {
    setEmailError(null);
    const parsed = parseSingleInviteEmail(email);
    if (!parsed.ok) {
      setEmailError(parsed.message);
      toast({
        variant: "destructive",
        title: "Invalid email",
        description: parsed.message,
      });
      return;
    }
    if (
      currentUserEmail &&
      parsed.email &&
      parsed.email.trim().toLowerCase() === currentUserEmail.trim().toLowerCase()
    ) {
      const message = "You cannot invite yourself to this datapack.";
      setEmailError(message);
      toast({ variant: "destructive", title: "Invalid email", description: message });
      return;
    }
    setSubmitting(true);
    try {
      await inviteDatapackShare(datapackId, {
        recipient_email: parsed?.email?.toLowerCase(),
        grant: inviteRole,
      }, {});
      setEmail("");
      toast({ title: "Invite sent" });
      await onInviteSent();
    } catch (err) {
      toast({
        variant: "destructive",
        title: "Invite failed",
        description: getApiErrorMessage(err, "Could not send invite."),
      });
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-end">
        <div className="min-w-0 flex-1 space-y-2">
          <label htmlFor="share-invite-emails" className="sr-only">
            Email address
          </label>
          <Input
            id="share-invite-emails"
            type="email"
            inputMode="email"
            autoComplete="email"
            placeholder="name@company.com"
            value={email}
            onChange={(e) => {
              setEmail(e.target.value);
              if (emailError) setEmailError(null);
            }}
            className={cn("bg-background/80", emailError && "border-destructive/60")}
            disabled={submitting}
          />
          {emailError ? <p className="text-xs text-destructive">{emailError}</p> : null}
        </div>
        <div className="flex w-full flex-col gap-2 sm:flex-row sm:items-center lg:w-auto">
          <Select
            value={inviteRole}
            onValueChange={(v) => {
              const id = v as ShareInviteRoleId;
              setInviteRole(id);
              setTileRole(id);
            }}
            disabled={submitting}
          >
            <SelectTrigger className="w-full sm:w-[200px] bg-background/80">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              {SHARE_INVITE_ROLES.map((r) => (
                <SelectItem key={r.id} value={r.id}>
                  {r.label}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
            type="button"
            disabled={submitting}
            className="w-full shrink-0 bg-primary text-primary-foreground sm:w-auto"
            onClick={() => void handleSend()}
          >
            {submitting ? "Sending…" : "Send invite"}
          </Button>
        </div>
      </div>

      <div className="mt-6 grid gap-3 sm:grid-cols-3">
        {SHARE_INVITE_ROLES.map((r) => {
          const Icon = r.icon;
          const selected = tileRole === r.id;
          return (
            <button
              key={r.id}
              type="button"
              disabled={submitting}
              onClick={() => syncSelectFromTiles(r.id)}
              className={cn(
                "flex flex-col gap-2 rounded-xl border p-4 text-left transition-colors",
                selected
                  ? "border-blue-500/60 bg-blue-500/10 shadow-[inset_0_0_0_1px_rgba(59,130,246,0.35)]"
                  : "border-border bg-muted/20 hover:border-muted-foreground/30 hover:bg-muted/30",
                submitting && "pointer-events-none opacity-60",
              )}
            >
              <Icon className={cn("h-5 w-5", selected ? "text-blue-400" : "text-muted-foreground")} aria-hidden />
              <span className="text-sm font-semibold text-foreground">{r.label}</span>
              <span className="text-xs leading-snug text-muted-foreground">{r.description}</span>
            </button>
          );
        })}
      </div>
    </div>
  );
}
