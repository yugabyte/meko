import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  API_ERROR_CODE_INVITE_PASSWORD_EXPIRED,
  API_ERROR_CODE_USER_ALREADY_CONFIRMED,
  API_ERROR_CODE_USER_NOT_CONFIRMED,
  getApiErrorMessage,
  getCurrentUser,
  isMekoApiRequestError,
  isNewPasswordChallengeResponse,
  login,
  ONBOARDING_STATUS_CACHE_KEY,
  ONBOARDING_STATUS_UPDATED_EVENT,
  resendSignupConfirmation,
} from "@/lib/api/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { AUTH_METHODS, TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { identifyUser, trackEvent } from "@/lib/telemetry/rudderstack";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import { setLocalItem } from "@/lib/browserStorage";
import { ROUTES } from "@/app/routes/paths";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";
import { GoogleIcon } from "@/components/icons/GoogleIcon";
import { LinkedInIcon } from "@/components/icons/LinkedInIcon";

const MEKO_REQUEST_ACCESS_URL = "https://info.yugabyte.com/meko-request-access" as const;

// Base for app-driven OAuth redirects. Defaults to the same-origin /api/v1 the
// rest of the SPA uses, so the Google sign-in redirect targets the api_server.
const API_BASE = (import.meta.env.VITE_MEKO_API_BASE_URL || "/api/v1").replace(/\/$/, "");

// Friendly messages for the ?error=<reason> codes a social callback appends
// when it bounces back to /signin. <provider>_* are app-internal reasons; the
// rest are OAuth2 codes Cognito/the IdP may forward.
const OAUTH_ERROR_REASONS: Record<string, string> = {
  google_state: "Your Google sign-in session expired. Please try again.",
  google_no_code: "Google sign-in didn't complete. Please try again.",
  google_exchange: "We couldn't finish signing you in with Google. Please try again.",
  google_provision: "We couldn't finish setting up your account. Please try again.",
  linkedin_state: "Your LinkedIn sign-in session expired. Please try again.",
  linkedin_no_code: "LinkedIn sign-in didn't complete. Please try again.",
  linkedin_exchange: "We couldn't finish signing you in with LinkedIn. Please try again.",
  linkedin_provision: "We couldn't finish setting up your account. Please try again.",
  access_denied: "Sign-in was cancelled.",
};

/** Query flag used with {@link ROUTES.signup} so the sign-up page opens on the “enter code” step. */
const SIGNUP_RESUME_CONFIRM_QUERY = "resume" as const;
const SIGNUP_RESUME_CONFIRM_VALUE = "1" as const;

function confirmSignupResumePath(email: string): string {
  const q = new URLSearchParams({
    [SIGNUP_RESUME_CONFIRM_QUERY]: SIGNUP_RESUME_CONFIRM_VALUE,
    email: email.trim(),
  });
  return `${ROUTES.signup}?${q.toString()}`;
}

export default function SignInPage() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [loading, setLoading] = useState(false);
  const [googleLoading, setGoogleLoading] = useState(false);
  const [linkedinLoading, setLinkedinLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** Set only after Cognito returns “user not confirmed” for this sign-in attempt. */
  const [showUnconfirmedRecovery, setShowUnconfirmedRecovery] = useState(false);
  const [resendLoading, setResendLoading] = useState(false);
  const [resendMessage, setResendMessage] = useState<string | null>(null);
  const [resendError, setResendError] = useState<string | null>(null);
  const [showPasswordResetBanner, setShowPasswordResetBanner] = useState(false);
  /** Set after a failed login for an admin-created user whose temp password expired. */
  const [showInviteExpired, setShowInviteExpired] = useState(false);

  useEffect(() => {
    if (searchParams.get("reset") !== "success") return;
    setShowPasswordResetBanner(true);
    const next = new URLSearchParams(searchParams);
    next.delete("reset");
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams]);

  // Surface ?error=<reason> appended by the Google callback when sign-in fails,
  // then strip it from the URL so a refresh doesn't re-show the message.
  useEffect(() => {
    const reason = searchParams.get("error");
    if (!reason) return;
    setError(OAUTH_ERROR_REASONS[reason] ?? "Sign in failed. Please try again.");
    const next = new URLSearchParams(searchParams);
    next.delete("error");
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams]);

  const handleGoogleSignIn = () => {
    if (loading || googleLoading || linkedinLoading) return;
    // Fires on click, before the redirect to the IdP — the sign-in isn't
    // confirmed yet (user may cancel/fail), so USER_SIGNED_IN would be
    // premature. login_method distinguishes the provider.
    trackEvent(TELEMETRY_EVENTS.SIGN_IN_CLICKED, {
      login_method: AUTH_METHODS.GOOGLE_OAUTH,
    });
    setError(null);
    setGoogleLoading(true);
    // App-driven Google sign-in: hand off to the api_server, which builds the
    // Cognito /oauth2/authorize redirect (identity_provider=Google).
    window.location.href = `${API_BASE}/auth/google/login`;
  };

  const handleLinkedInSignIn = () => {
    if (loading || googleLoading || linkedinLoading) return;
    // Click-time event (pre-redirect), mirroring the Google handler.
    trackEvent(TELEMETRY_EVENTS.SIGN_IN_CLICKED, {
      login_method: AUTH_METHODS.LINKEDIN_OAUTH,
    });
    setError(null);
    setLinkedinLoading(true);
    // App-driven LinkedIn sign-in: hand off to the api_server, which builds
    // the Cognito /oauth2/authorize redirect (identity_provider=LinkedIn).
    window.location.href = `${API_BASE}/auth/linkedin/login`;
  };

  const resetRecoveryUi = () => {
    setShowUnconfirmedRecovery(false);
    setResendMessage(null);
    setResendError(null);
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError(null);
    setShowInviteExpired(false);
    resetRecoveryUi();
    try {
      const loginResult = await login({ email: email.trim(), password });
      if (isNewPasswordChallengeResponse(loginResult)) {
        navigate(ROUTES.completeNewPassword, {
          replace: true,
          state: { session: loginResult.session, email: loginResult.email },
        });
        return;
      }
      let nextPath: (typeof ROUTES)["datapacks" | "onboarding"] = ROUTES.onboarding;
      try {
        const user = await getCurrentUser();
        identifyUser({
          id: user.account_id,
          email: user.email,
          firstName: user.first_name,
          lastName: user.last_name,
          userType: user.user_type,
        });
        trackEvent(TELEMETRY_EVENTS.USER_SIGNED_IN, {
          email: user.email,
          login_method: AUTH_METHODS.EMAIL_PASSWORD,
        });
        setLocalItem(ONBOARDING_STATUS_CACHE_KEY, String(user.has_completed_onboarding));
        dispatchAppEvent(ONBOARDING_STATUS_UPDATED_EVENT, {
          has_completed_onboarding: user.has_completed_onboarding,
        });
        if (user.has_completed_onboarding) {
          nextPath = ROUTES.datapacks;
        }
      } catch {
        // Unusual after a successful login; still land on onboarding so the user isn’t stuck.
      }
      navigate(nextPath, { replace: true });
    } catch (err) {
      const code = isMekoApiRequestError(err) ? err.code : undefined;
      if (code === API_ERROR_CODE_INVITE_PASSWORD_EXPIRED) {
        // Admin-created account whose temp password expired. The server already
        // promoted it to CONFIRMED, so direct the user to Forgot Password (which
        // now works) via a banner instead of a generic error.
        setShowInviteExpired(true);
        return;
      }
      setShowUnconfirmedRecovery(code === API_ERROR_CODE_USER_NOT_CONFIRMED);
      setError(getApiErrorMessage(err, "Sign in failed. Please try again."));
    } finally {
      setLoading(false);
    }
  };

  const handleResendCode = async () => {
    const trimmed = email.trim();
    if (!trimmed) return;
    setResendLoading(true);
    setResendError(null);
    setResendMessage(null);
    try {
      await resendSignupConfirmation({ email: trimmed });
      setResendMessage("Check your inbox for a new verification code.");
    } catch (err) {
      // Cognito returns USER_ALREADY_CONFIRMED (HTTP 409) when the account is
      // already verified — that's not an error from the user's POV, so
      // collapse the recovery panel and clear the failed-login banner so they
      // can just retype their password and sign in.
      if (isMekoApiRequestError(err) && err.code === API_ERROR_CODE_USER_ALREADY_CONFIRMED) {
        resetRecoveryUi();
        setError("This email is already verified. Please sign in with your password.");
        return;
      }
      setResendError(getApiErrorMessage(err, "Could not resend the code. Try again later."));
    } finally {
      setResendLoading(false);
    }
  };

  return (
    <AuthPageShell>
      {/* Theme toggle pinned top-right so unauthenticated users (who don't
          see the workspace header) can still flip between light and dark
          modes before signing in. `fixed` is screen-relative, so it stays
          put regardless of card height / scroll. */}
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggleButton />
      </div>
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-card p-8 shadow-sm">
        <h1 className="mb-6 text-center text-3xl font-semibold text-foreground">Sign in to Meko</h1>

        {showPasswordResetBanner ? (
          <div className="mb-4 rounded-lg border border-emerald-500/30 bg-emerald-950/25 px-4 py-3 text-sm text-emerald-700 dark:text-emerald-100">
            Your password was updated. Sign in with your new password.
          </div>
        ) : null}

        {showInviteExpired ? (
          <div className="mb-4 rounded-lg border border-amber-500/30 bg-amber-950/25 px-4 py-3 text-sm text-amber-700 dark:text-amber-100">
            Your temporary password has expired. We&apos;ve emailed you a new one — check your inbox
            and sign in again.
          </div>
        ) : null}

        <form className="space-y-4" onSubmit={handleSubmit}>
          <Input
            type="email"
            autoComplete="username"
            name="email"
            aria-label="Email address"
            inputMode="email"
            autoCapitalize="none"
            spellCheck={false}
            enterKeyHint="next"
            placeholder="Your email address"
            value={email}
            onChange={(e) => {
              setEmail(e.target.value);
              if (showUnconfirmedRecovery) {
                resetRecoveryUi();
                setError(null);
              }
            }}
            required
            className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
          />
          <Input
            type="password"
            autoComplete="current-password"
            name="password"
            aria-label="Password"
            enterKeyHint="go"
            placeholder="Your password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
            className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
          />
          <Button
            type="submit"
            className="h-12 w-full bg-primary text-primary-foreground hover:bg-primary/90"
            disabled={loading || googleLoading || linkedinLoading}
          >
            {loading ? "Please wait..." : "Continue"}
          </Button>
        </form>

        <div className="my-5 flex items-center gap-3">
          <div className="h-px flex-1 bg-border" />
          <span className="text-xs text-muted-foreground">or</span>
          <div className="h-px flex-1 bg-border" />
        </div>

        <div className="flex flex-col gap-3">
          <Button
            type="button"
            variant="outline"
            className="h-12 w-full border-border bg-input text-foreground hover:bg-white/10"
            disabled={loading || googleLoading || linkedinLoading}
            onClick={handleGoogleSignIn}
          >
            <GoogleIcon className="mr-2 h-5 w-5" />
            {googleLoading ? "Redirecting to Google…" : "Continue with Google"}
          </Button>

          <Button
            type="button"
            variant="outline"
            className="h-12 w-full border-border bg-input text-foreground hover:bg-white/10"
            disabled={loading || googleLoading || linkedinLoading}
            onClick={handleLinkedInSignIn}
          >
            <LinkedInIcon className="mr-2 h-5 w-5 text-[#0A66C2] dark:text-white" />
            {linkedinLoading ? "Redirecting to LinkedIn…" : "Continue with LinkedIn"}
          </Button>
        </div>

        {error ? <p className="mt-4 text-sm text-red-600">{error}</p> : null}

        {showUnconfirmedRecovery ? (
          <div className="mt-5 rounded-lg border border-amber-500/25 bg-amber-950/20 px-4 py-3 text-sm text-amber-50/95">
            <p className="font-medium text-amber-700 dark:text-amber-100">
              Finish verifying this email
            </p>
            <p className="mt-1.5 text-xs leading-relaxed text-amber-700 dark:text-amber-100/80">
              Your account exists but the email is not verified yet. Resend the code to verify your
              email.
            </p>
            <div className="mt-3 flex flex-col gap-2 sm:flex-row sm:flex-wrap">
              <Button
                type="button"
                variant="secondary"
                className="h-9 bg-white/10 text-white hover:bg-white/15"
                disabled={resendLoading || !email.trim()}
                onClick={() => void handleResendCode()}
              >
                {resendLoading ? "Sending…" : "Resend verification email"}
              </Button>
              <Button
                type="button"
                variant="outline"
                className="h-9 border-amber-500/40 bg-transparent text-amber-50 hover:bg-amber-950/40"
                asChild
              >
                <Link to={confirmSignupResumePath(email)}>Enter verification code</Link>
              </Button>
            </div>
            {resendMessage ? (
              <p className="mt-2 text-xs text-emerald-700 dark:text-emerald-300/90">
                {resendMessage}
              </p>
            ) : null}
            {resendError ? (
              <p className="mt-2 text-xs text-red-700 dark:text-red-300">{resendError}</p>
            ) : null}
          </div>
        ) : null}

        <div className="mt-5 flex flex-col gap-2 text-center text-sm text-muted-foreground">
          <Link to={ROUTES.forgotPassword} className="font-medium text-blue-400 hover:underline">
            Forgot password?
          </Link>
          <p>
            New to Meko?{" "}
            <Link to={ROUTES.signup} className="font-medium text-blue-400 hover:underline">
              Sign up
            </Link>
          </p>
        </div>
      </div>
    </AuthPageShell>
  );
}
