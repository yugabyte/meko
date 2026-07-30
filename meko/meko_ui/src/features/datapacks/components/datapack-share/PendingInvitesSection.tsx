// Lists pending datapack share invites and wires Resend / Revoke to the invite API endpoints.
// Parent supplies pending invite rows (GET outgoing-shares); on success we toast and call onChanged to refresh lists.
import { useState } from "react";
import { Clock, Mail } from "lucide-react";
import { Button } from "@/components/ui/button";
import { toast } from "@/hooks/use-toast";
import {
  getApiErrorMessage,
  resendDatapackInvite,
  revokeDatapackInvite,
  type DatapackOutgoingPendingInvite,
} from "@/lib/api/mekoApi";
import { shareGrantDescription, shareGrantLabel } from "@/features/datapacks/components/datapack-share/shareInviteRoles";

type Props = {
  datapackId: string;
  invites: DatapackOutgoingPendingInvite[];
  loading?: boolean;
  onChanged: () => void | Promise<void>;
};

/** Tracks which invite row is in-flight so only that row’s buttons show a spinner. */
type Busy = { id: string; action: "resend" | "revoke" };

export function PendingInvitesSection({ datapackId, invites, loading = false, onChanged }: Props) {
  const [busy, setBusy] = useState<Busy | null>(null);

  const run = async (inviteId: string, action: "resend" | "revoke") => {
    setBusy({ id: inviteId, action });
    try {
      if (action === "resend") {
        await resendDatapackInvite(datapackId, inviteId, {});
        toast({ title: "Invite resent" });
      } else {
        await revokeDatapackInvite(datapackId, inviteId, {});
        toast({ title: "Invite revoked" });
      }
      await onChanged();
    } catch (err) {
      toast({
        variant: "destructive",
        title: action === "resend" ? "Could not resend" : "Could not revoke",
        description: getApiErrorMessage(err, "Something went wrong. Try again."),
      });
    } finally {
      setBusy(null);
    }
  };

  const count = invites.length;
  const isBusy = (id: string, action: "resend" | "revoke") =>
    busy?.id === id && busy?.action === action;

  return (
    <div className="rounded-xl border border-border bg-card/50 shadow-sm">
      <div className="flex items-center gap-2 border-b border-border px-4 py-3">
        <Clock className="h-4 w-4 shrink-0 text-amber-400" aria-hidden />
        <span className="text-xs font-semibold uppercase tracking-wide text-amber-700 dark:text-amber-200/90">
          Pending · {loading ? "…" : count}
        </span>
      </div>
      <div className="p-4 pt-3">
        {loading && invites.length === 0 ? (
          <div className="space-y-2">
            <div className="h-[72px] animate-pulse rounded-lg border border-border bg-muted/20" />
          </div>
        ) : count === 0 ? (
          <p className="rounded-lg border border-dashed border-border/60 bg-muted/5 px-4 py-8 text-center text-sm text-muted-foreground">
            No pending invites.
          </p>
        ) : (
          <ul className="space-y-2">
            {invites.map((inv) => {
              const primary = inv.recipient_email_id || inv.recipient_user_name || "Invitee";
              const rowBusy = busy?.id === inv.invite_id;
              return (
                <li
                  key={inv.invite_id}
                  className="flex flex-wrap items-center gap-3 rounded-lg border border-border bg-background/50 px-3 py-3"
                >
                  <div
                    className="flex h-10 w-10 shrink-0 items-center justify-center rounded-full bg-muted text-muted-foreground"
                    aria-hidden
                  >
                    <Mail className="h-5 w-5" />
                  </div>
                  <div className="min-w-0 flex-1">
                    <div className="truncate text-sm font-medium text-foreground">{primary}</div>
                    <div className="space-y-0.5">
                      <div className="text-xs text-muted-foreground">
                        Invited · {shareGrantLabel(inv.grant)}
                      </div>
                    </div>
                  </div>
                  <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-8 text-muted-foreground hover:text-foreground"
                      disabled={rowBusy}
                      onClick={() => void run(inv.invite_id, "resend")}
                    >
                      {isBusy(inv.invite_id, "resend") ? "…" : "Resend"}
                    </Button>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-8 text-muted-foreground hover:text-foreground"
                      disabled={rowBusy}
                      onClick={() => void run(inv.invite_id, "revoke")}
                    >
                      {isBusy(inv.invite_id, "revoke") ? "…" : "Revoke"}
                    </Button>
                  </div>
                </li>
              );
            })}
          </ul>
        )}
      </div>
    </div>
  );
}
