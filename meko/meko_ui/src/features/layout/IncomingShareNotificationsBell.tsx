// Bell + popover that loads pending datapack share invites and posts accept/decline via invite-action.
import { useCallback, useEffect, useState } from "react";
import { Bell } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";
import { getSessionItem, setSessionItem } from "@/lib/browserStorage";
import {
  getApiErrorMessage,
  listDatapackIncomingShares,
  postDatapackInviteAction,
  type DatapackIncomingShare,
} from "@/lib/api/mekoApi";
import { notifyDatapacksListChanged } from "@/features/observe/observeRefresh";
import { shareGrantLabel } from "@/features/datapacks/components/datapack-share/shareInviteRoles";
import { TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { trackEvent } from "@/lib/telemetry/rudderstack";
import { nowIso } from "@/lib/telemetry/helpers";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { toast } from "@/hooks/use-toast";
import {
  formatIncomingShareInviteMessage,
  INCOMING_SHARE_ACCEPT_LABEL,
  INCOMING_SHARE_DECLINE_LABEL,
  INCOMING_SHARE_NOTIFICATION_ARIA,
  INCOMING_SHARE_NOTIFICATION_EMPTY,
  INCOMING_SHARE_NOTIFICATION_LOADING,
  INCOMING_SHARE_NOTIFICATION_TITLE,
  INCOMING_SHARE_TOAST_ACCEPTED_TITLE,
  INCOMING_SHARE_TOAST_ACTION_FAILED_TITLE,
  INCOMING_SHARE_TOAST_DECLINED_TITLE,
} from "@/features/layout/incomingShareNotificationsConstants";

type Props = {
  /** Logged-in account id (same value the API expects as `recipient_user_id` on invite-action). */
  recipientUserId: string;
};

// sessionStorage flag: "has this tab auto-opened the bell once already?"
// We auto-pop the popover on first load when there are pending invites so
// the user actually notices them, but we don't want it springing back open
// on every re-render or after a dismiss. Per-tab (not per-origin) so a
// fresh tab still gets the heads-up.
const AUTO_OPENED_STORAGE_KEY = "meko:notifications-bell:auto-opened-this-session";

export function IncomingShareNotificationsBell({ recipientUserId }: Props) {
  const { data: currentUser } = useCurrentUser();
  const [open, setOpen] = useState(false);
  const [invites, setInvites] = useState<DatapackIncomingShare[]>([]);
  const [loading, setLoading] = useState(false);
  const [busy, setBusy] = useState<{ inviteId: string; action: "accept" | "decline" } | null>(null);

  const loadInvites = useCallback(async () => {
    setLoading(true);
    try {
      const rows = await listDatapackIncomingShares();
      setInvites(rows ?? []);
    } catch {
      setInvites([]);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadInvites();
  }, [loadInvites]);

  // Auto-open the popover on first load when there's at least one pending
  // invite — the bell badge alone is easy to miss. Gated by a sessionStorage
  // flag so dismissing the popover (or any subsequent re-renders / refetches
  // in the same tab) doesn't pop it back open. Cleared automatically when
  // the tab closes, so a fresh tab gets the heads-up again.
  useEffect(() => {
    if (loading) return;
    if (invites.length === 0) return;
    if (getSessionItem(AUTO_OPENED_STORAGE_KEY) === "1") return;
    setOpen(true);
    setSessionItem(AUTO_OPENED_STORAGE_KEY, "1");
  }, [loading, invites.length]);

  const runAction = async (invite: DatapackIncomingShare, user_action: "accept" | "decline") => {
    setBusy({ inviteId: invite.invite_id, action: user_action });
    try {
      await postDatapackInviteAction({
        invite_id: invite.invite_id,
        recipient_user_id: recipientUserId,
        user_action,
      });
      setInvites((prev) => prev.filter((r) => r.invite_id !== invite.invite_id));
      if (user_action === "accept") {
        trackEvent(TELEMETRY_EVENTS.INVITE_ACCEPTED, {
          datapack_id: invite.datapack_id,
          recipient_email: currentUser?.email,
          recipient_role: invite.grant,
          // shared_by is the datapack owner shown in the invite ("Raj added you…").
          // Role is intentionally omitted — the invite doesn't record the actual
          // sharer (a maintainer can share), so a role here would be misleading.
          shared_by_email: invite.owner_email,
          shared_by_user_id: invite.owner_user_id,
          accepted_timestamp: nowIso(),
        });
        notifyDatapacksListChanged(`share-accepted:${invite.datapack_name || invite.invite_id}`);
      }
      toast({
        title: user_action === "accept" ? INCOMING_SHARE_TOAST_ACCEPTED_TITLE : INCOMING_SHARE_TOAST_DECLINED_TITLE,
      });
      // Re-sync from the server so the badge count reflects BE truth
      // (and the row doesn't pop back on next mount if the BE is still
      // returning the accepted/declined invite).
      void loadInvites();
    } catch (err) {
      toast({
        variant: "destructive",
        title: INCOMING_SHARE_TOAST_ACTION_FAILED_TITLE,
        description: getApiErrorMessage(err, "Something went wrong. Try again."),
      });
    } finally {
      setBusy(null);
    }
  };

  const count = invites.length;

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next);
        if (next) void loadInvites();
      }}
    >
      <PopoverTrigger asChild>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="relative h-11 w-11 shrink-0 text-muted-foreground hover:text-foreground"
          aria-label={INCOMING_SHARE_NOTIFICATION_ARIA}
        >
          <Bell className="h-7 w-7 shrink-0" strokeWidth={2} aria-hidden />
          {count > 0 ? (
            <span className="absolute -right-0.5 -top-0.5 z-[1] flex h-[18px] min-w-[18px] items-center justify-center rounded-full border-2 border-background bg-primary px-1 text-[10px] font-semibold leading-none text-primary-foreground shadow-sm">
              {count > 9 ? "9+" : count}
            </span>
          ) : null}
        </Button>
      </PopoverTrigger>
      <PopoverContent align="end" className="w-[min(22rem,calc(100vw-2rem))] p-0" sideOffset={8}>
        <div className="border-b border-border px-3 py-2">
          <p className="text-sm font-semibold text-foreground">{INCOMING_SHARE_NOTIFICATION_TITLE}</p>
        </div>
        <div className="max-h-[min(24rem,70vh)] overflow-y-auto p-2">
          {loading ? (
            <p className="px-2 py-6 text-center text-sm text-muted-foreground">{INCOMING_SHARE_NOTIFICATION_LOADING}</p>
          ) : invites.length === 0 ? (
            <p className="px-2 py-6 text-center text-sm text-muted-foreground">{INCOMING_SHARE_NOTIFICATION_EMPTY}</p>
          ) : (
            <ul className="space-y-2">
              {invites.map((inv) => {
                const grantLabel = shareGrantLabel(inv.grant);
                const line = formatIncomingShareInviteMessage(inv.owner_name, grantLabel, inv.datapack_name);
                const rowBusy = busy?.inviteId === inv.invite_id;
                const acceptBusy = rowBusy && busy?.action === "accept";
                const declineBusy = rowBusy && busy?.action === "decline";
                return (
                  <li
                    key={inv.invite_id}
                    className="rounded-lg border border-border bg-muted/20 px-3 py-3 text-sm text-foreground"
                  >
                    <p className="leading-snug">{line}</p>
                    <div className={cn("mt-3 flex flex-wrap gap-2", rowBusy && "opacity-70")}>
                      <Button
                        type="button"
                        size="sm"
                        className="h-8 bg-primary text-primary-foreground hover:bg-primary/90"
                        disabled={busy !== null}
                        onClick={() => void runAction(inv, "accept")}
                      >
                        {acceptBusy ? "…" : INCOMING_SHARE_ACCEPT_LABEL}
                      </Button>
                      <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        className="h-8"
                        disabled={busy !== null}
                        onClick={() => void runAction(inv, "decline")}
                      >
                        {declineBusy ? "…" : INCOMING_SHARE_DECLINE_LABEL}
                      </Button>
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </PopoverContent>
    </Popover>
  );
}
