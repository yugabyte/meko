// Signed-out side of /share/:token: says a datapack was shared, with sign up / sign
// in links that bring the visitor back here to join. The datapack isn't named until sign-in.
import { Link, useParams } from "react-router-dom";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import { Button } from "@/components/ui/button";
import { ROUTES } from "@/app/routes/paths";
import { savePendingShareToken } from "@/lib/pendingShareLink";

export default function ShareLinkLandingPage() {
  const token = useParams<{ token: string }>().token ?? "";
  // The token goes in sessionStorage, not in a return_to query: analytics records
  // page URLs, and sign-in resumes from the pending link. Saved only once the
  // visitor chooses to continue.
  const remember = () => savePendingShareToken(token);

  return (
    <AuthPageShell>
      <div className="w-full max-w-[560px] space-y-6 rounded-2xl border border-border bg-card p-8 text-center shadow-sm">
        <div className="space-y-2">
          <h1 className="text-3xl font-semibold text-foreground">A datapack was shared with you</h1>
          <p className="text-sm text-muted-foreground">Sign in or create an account to open it.</p>
        </div>
        <div className="flex flex-col gap-2 sm:flex-row sm:justify-center">
          <Button asChild>
            <Link to={ROUTES.signup} onClick={remember}>
              Sign up to open it
            </Link>
          </Button>
          <Button asChild variant="outline">
            <Link to={ROUTES.signin} onClick={remember}>
              Sign in
            </Link>
          </Button>
        </div>
      </div>
    </AuthPageShell>
  );
}
