// Account settings page — read-only profile info (account_id, email,
// first/last name) plus the entry point to the change-password modal.
//
// Profile fields come from `/auth/me` (same endpoint the AppWorkspaceHeader
// uses), so the data is already cached in most cases. Re-fetched here on
// mount so deep-links into `/settings` from external nav still work without
// requiring the header to have loaded first.

import { useState } from "react";
import { Copy, KeyRound, Loader2, Mail, User as UserIcon } from "lucide-react";
import { Button } from "@/components/ui/button";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { UpdatePasswordDialog } from "@/features/settings/components/UpdatePasswordDialog";
import { SecurityTokensSection } from "@/features/settings/components/SecurityTokensSection";
import { PricingSection } from "@/features/settings/components/PricingSection";

export default function SettingsPage() {
  // Shared, session-cached identity — usually already warmed by the
  // header, so deep-linking into /settings reads from cache.
  const { data: user = null, isLoading: loading, error } = useCurrentUser();
  const errorMessage = error ? getApiErrorMessage(error, "Failed to load your account.") : null;
  const [passwordDialogOpen, setPasswordDialogOpen] = useState(false);

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
        <div className="mx-auto w-full max-w-2xl space-y-6">
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
