// Account settings page — read-only profile info (account_id, email,
// first/last name) plus the entry point to the change-password modal.
//
// Profile fields come from `/auth/me` (same endpoint the AppWorkspaceHeader
// uses), so the data is already cached in most cases. Re-fetched here on
// mount so deep-links into `/settings` from external nav still work without
// requiring the header to have loaded first.

import { useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import {
  ArrowUpRight,
  Copy,
  KeyRound,
  Loader2,
  LifeBuoy,
  Mail,
  Trash2,
  User as UserIcon,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { queryClient } from "@/lib/queryClient";
import { ACCOUNT_USAGE_QUERY_KEY } from "@/hooks/useAccountUsage";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { UpdatePasswordDialog } from "@/features/settings/components/UpdatePasswordDialog";
import { SecurityTokensSection } from "@/features/settings/components/SecurityTokensSection";
import { PricingSection } from "@/features/settings/components/PricingSection";

// Where users go to request account deletion. There is no self-serve
// delete flow today — a support engineer verifies identity and reason
// before the account + owned datapacks are removed, so the CTA opens a
// support ticket instead of firing a DELETE call. Single constant so a
// future URL change is one edit.
const SUPPORT_URL = "https://support.yugabyte.com/";

export default function SettingsPage() {
  // Shared, session-cached identity — usually already warmed by the
  // header, so deep-linking into /settings reads from cache.
  const { data: user = null, isLoading: loading, error } = useCurrentUser();
  const errorMessage = error ? getApiErrorMessage(error, "Failed to load your account.") : null;
  const [passwordDialogOpen, setPasswordDialogOpen] = useState(false);
  const [deleteAccountDialogOpen, setDeleteAccountDialogOpen] = useState(false);

  // AWS Marketplace welcome landing. api_server's /marketplace/register
  // redirects the buyer here with `?source=aws-marketplace&plan=<tier>`
  // once the entitlement is persisted. Show a one-shot toast + refetch
  // usage so the new tier is reflected immediately, then strip the
  // query params so a page reload doesn't re-toast.
  const location = useLocation();
  const navigate = useNavigate();
  useEffect(() => {
    const q = new URLSearchParams(location.search);
    if (q.get("source") !== "aws-marketplace") return;
    const plan = q.get("plan") ?? "";
    toast({
      title: "You're subscribed via AWS Marketplace",
      description: plan
        ? `Your Meko workspace is now on the ${plan} plan.`
        : "Your Meko workspace has been upgraded.",
    });
    void queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] });
    q.delete("source");
    q.delete("plan");
    navigate({ pathname: location.pathname, search: q.toString() }, { replace: true });
  }, [location.pathname, location.search, navigate]);

  // Convenience helper for the "copy account id" button — UUIDs are awkward
  // to retype, and support / debugging frequently asks for them.
  const copyAccountId = async () => {
    if (!user?.account_id) return;
    try {
      await navigator.clipboard.writeText(user.account_id);
      toast({ title: "Account ID copied" });
    } catch {
      toast({
        title: "Couldn't copy",
        description: "Browser blocked clipboard access.",
      });
    }
  };

  const fullName = user ? [user.first_name, user.last_name].filter(Boolean).join(" ").trim() : "";

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader
        leading={<h1 className="text-base font-medium text-foreground">Settings</h1>}
      />

      {/* Tight padding on mobile (16px) so content gets ~340px of a
          375px viewport; restore the spacious 32px gutter at md+. */}
      <div className="flex-1 overflow-y-auto px-4 py-5 sm:px-6 md:px-8 md:py-8">
        <div className="mx-auto w-full max-w-5xl space-y-6">
          {loading ? (
            <div className="flex items-center gap-2 text-sm text-muted-foreground">
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
              Loading your account…
            </div>
          ) : errorMessage ? (
            <div className="rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-300">
              {errorMessage}
            </div>
          ) : user ? (
            <>
              {/* Profile section — read-only for now; profile editing is
                  outside the scope of this first pass. */}
              <section className="rounded-xl border border-border bg-card p-5">
                <header className="mb-4">
                  <h2 className="text-sm font-semibold text-foreground">Profile</h2>
                </header>

                <dl className="space-y-3">
                  <Field
                    label="Name"
                    icon={
                      <UserIcon
                        className="h-3.5 w-3.5 text-blue-700 dark:text-blue-300"
                        aria-hidden
                      />
                    }
                    value={fullName || "—"}
                  />
                  <Field
                    label="Email"
                    icon={
                      <Mail
                        className="h-3.5 w-3.5 text-emerald-700 dark:text-emerald-300"
                        aria-hidden
                      />
                    }
                    value={user.email || "—"}
                  />
                  <Field
                    label="Account ID"
                    value={user.account_id || "—"}
                    mono
                    trailing={
                      user.account_id ? (
                        <Button
                          type="button"
                          variant="ghost"
                          size="sm"
                          onClick={copyAccountId}
                          className="h-7 gap-1.5 px-2 text-xs text-muted-foreground hover:text-foreground"
                          aria-label="Copy account ID"
                        >
                          <Copy className="h-3 w-3" aria-hidden />
                          Copy
                        </Button>
                      ) : null
                    }
                  />
                </dl>

                {/* Delete-account row — sibling to the <dl> (not inside
                    it) because it isn't a name/value pair, it's an
                    action; and rendered outside the `Field` grid so the
                    "Delete account" title doesn't fight Field's fixed
                    w-32 uppercase label column. Layout mirrors the
                    Update-password section: icon + title + description
                    on the left, action button on the right. Divider
                    keeps visual separation from the identity rows above
                    without dropping into a brand-new <section>. Button
                    uses the brand colour (not red) — this is a request
                    handed off to support, not a one-click destructive
                    action, so red would over-signal danger. */}
                <div className="mt-4 flex items-center justify-between gap-3 border-t border-border/40 pt-4">
                  <div className="flex min-w-0 items-start gap-2.5">
                    <Trash2
                      className="mt-0.5 h-4 w-4 shrink-0 text-brand"
                      aria-hidden
                    />
                    <div className="min-w-0">
                      <p className="text-sm font-medium text-foreground">Delete account</p>
                      <p className="mt-0.5 text-xs text-muted-foreground">
                        Deletion is handled by our support team.
                      </p>
                    </div>
                  </div>
                  {/* Primary CTA. Uses the `default` Button variant
                      (bg-primary / brand orange filled) — same pattern
                      as "Add Datapack" on the datapacks page and the
                      AWS-Marketplace "Upgrade" chip on the pricing
                      card. `hover:bg-primary/90` matches Add Datapack's
                      hover treatment for consistency. */}
                  <Button
                    type="button"
                    size="sm"
                    onClick={() => setDeleteAccountDialogOpen(true)}
                    className="shrink-0 gap-1.5 bg-primary text-primary-foreground hover:bg-primary/90"
                  >
                    Request deletion
                  </Button>
                </div>
              </section>

              {/* Security section — password + API keys. The tokens
                  subsection is its own component so the create/list
                  lifecycle and the plaintext-reveal state are isolated.

                  The password subsection is gated on has_email_password_login
                  from GET /auth/me (false for Google_ / LinkedIn_ / GitHub_
                  usernames). Optional on older BE deploys → show the form. */}
              {user.has_email_password_login !== false ? (
                <section className="rounded-xl border border-border bg-card p-5">
                  <header className="mb-4 flex items-start justify-between gap-3">
                    <div>
                      <h2 className="text-sm font-semibold text-foreground">Password</h2>
                      <p className="mt-0.5 text-xs text-muted-foreground">
                        Change the password for this account.
                      </p>
                    </div>
                    <Button
                      type="button"
                      variant="outline"
                      size="sm"
                      onClick={() => setPasswordDialogOpen(true)}
                      className="shrink-0 gap-1.5"
                    >
                      <KeyRound className="h-3.5 w-3.5" aria-hidden />
                      Update password
                    </Button>
                  </header>
                </section>
              ) : null}

              <SecurityTokensSection />

              <PricingSection />
            </>
          ) : null}
        </div>
      </div>

      <UpdatePasswordDialog
        open={passwordDialogOpen}
        onOpenChange={setPasswordDialogOpen}
        username={user?.email}
      />

      {/* Delete-account request dialog. The primary CTA opens a support
          ticket (new tab) rather than firing a self-serve DELETE — a
          support engineer verifies identity and the reason before any
          data is removed, so this dialog is deliberately a hand-off
          rather than a confirm-then-destroy flow. Copy is calm rather
          than alarmist because at this point nothing has happened yet. */}
      <Dialog open={deleteAccountDialogOpen} onOpenChange={setDeleteAccountDialogOpen}>
        <DialogContent className="sm:max-w-md">
          {/* Icon + title on one horizontal row (not stacked). The Radix
              DialogHeader defaults to `flex flex-col` so the trash icon
              was landing above the title on two lines; overriding with
              a row layout lands them side-by-side and stays consistent
              with the other primary-action dialogs in the app. */}
          <DialogHeader className="flex-row items-center gap-3 space-y-0">
            <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-brand/10">
              <Trash2 className="h-4 w-4 text-brand" aria-hidden />
            </div>
            <div className="min-w-0">
              <DialogTitle>Request account deletion</DialogTitle>
            </div>
          </DialogHeader>

          <DialogDescription>
            We handle account deletion through Yugabyte support so we can verify identity and
            remove your datapacks, memories, and knowledge base together.
          </DialogDescription>

          <div className="rounded-md border border-border/60 bg-muted/30 p-3 text-xs text-muted-foreground">
            <span className="font-medium text-foreground">Before you file the ticket:</span> mention
            the email <span className="font-mono text-foreground">{user?.email || "on your account"}</span>{" "}
            and your account ID so support can locate it faster.
          </div>

          <DialogFooter className="gap-2 sm:justify-end">
            <Button
              type="button"
              variant="outline"
              onClick={() => setDeleteAccountDialogOpen(false)}
            >
              Cancel
            </Button>
            {/* Primary CTA. `bg-primary text-primary-foreground`
                (same tokens `Add Datapack` and every other primary
                button on the app uses) — NOT `bg-brand text-brand-fg`.
                The two tokens map to the same orange fill, but
                `--brand-foreground` is WHITE in light mode
                (index.css:148) while `--primary-foreground` is navy —
                and only navy-on-orange passes WCAG AA (see the
                explanatory comment on `--primary-foreground` right
                above it). Sticking with `primary` here keeps every
                orange filled button in the app rendering the same
                dark-on-orange text. */}
            <Button
              asChild
              type="button"
              className="gap-1.5 bg-primary text-primary-foreground hover:bg-primary/90"
              onClick={() => setDeleteAccountDialogOpen(false)}
            >
              <a href={SUPPORT_URL} target="_blank" rel="noopener noreferrer">
                <LifeBuoy className="h-3.5 w-3.5" aria-hidden />
                Contact support
                <ArrowUpRight className="h-3.5 w-3.5" aria-hidden />
              </a>
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function Field({
  label,
  icon,
  value,
  mono,
  trailing,
}: {
  label: string;
  icon?: React.ReactNode;
  value: string;
  mono?: boolean;
  trailing?: React.ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-border/40 pb-3 last:border-0 last:pb-0">
      <dt className="inline-flex w-32 shrink-0 items-center gap-1.5 text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
        {icon}
        {label}
      </dt>
      <dd
        className={
          mono
            ? "min-w-0 flex-1 truncate font-mono text-xs text-foreground"
            : "min-w-0 flex-1 truncate text-sm text-foreground"
        }
        title={value}
      >
        {value}
      </dd>
      {trailing}
    </div>
  );
}
