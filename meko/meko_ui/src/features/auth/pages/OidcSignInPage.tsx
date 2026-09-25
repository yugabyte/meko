import { Button } from "@/components/ui/button";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";

interface OidcSignInPageProps {
  /** Where the api-server starts the flow; from GET /auth/config. */
  loginUrl: string;
  /** A ?error=<reason> the callback bounced back with, already made friendly. */
  error?: string | null;
}

/**
 * Sign-in against a generic OIDC issuer.
 *
 * One button, because that is genuinely all there is: the issuer owns its users,
 * so there is no password to type here, no account to create here, and no social
 * buttons — whether the issuer itself federates to Google or an LDAP directory is
 * its configuration and invisible from this page.
 *
 * A full navigation rather than fetch(): the flow is a 302 to the issuer, and
 * XHR cannot follow a cross-origin redirect that sets cookies.
 */
export default function OidcSignInPage({ loginUrl, error }: OidcSignInPageProps) {
  return (
    <AuthPageShell>
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggleButton />
      </div>
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-card p-8 shadow-sm">
        <h1 className="mb-2 text-center text-3xl font-semibold text-foreground">Sign in to Meko</h1>
        <p className="mb-6 text-center text-sm text-muted-foreground">
          You&apos;ll be redirected to your organization&apos;s identity provider.
        </p>

        {error ? <p className="mb-4 text-center text-sm text-red-600">{error}</p> : null}

        <Button
          type="button"
          className="h-12 w-full"
          onClick={() => {
            window.location.assign(loginUrl);
          }}
        >
          Continue to sign in
        </Button>
      </div>
    </AuthPageShell>
  );
}
