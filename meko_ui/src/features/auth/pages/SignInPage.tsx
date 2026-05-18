import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
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
} from "@/lib/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { ROUTES } from "@/app/routes/paths";

const MEKO_REQUEST_ACCESS_URL = "https://info.yugabyte.com/meko-request-access" as const;

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
  const [error, setError] = useState<string | null>(null);
  /** Set only after Cognito returns “user not confirmed” for this sign-in attempt. */
  const [showUnconfirmedRecovery, setShowUnconfirmedRecovery] = useState(false);
  const [resendLoading, setResendLoading] = useState(false);
  const [resendMessage, setResendMessage] = useState<string | null>(null);
  const [resendError, setResendError] = useState<string | null>(null);
  const [showPasswordResetBanner, setShowPasswordResetBanner] = useState(false);

  useEffect(() => {
    if (searchParams.get("reset") !== "success") return;
    setShowPasswordResetBanner(true);
    const next = new URLSearchParams(searchParams);
    next.delete("reset");
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams]);

  const resetRecoveryUi = () => {
    setShowUnconfirmedRecovery(false);
    setResendMessage(null);
    setResendError(null);
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError(null);
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
        localStorage.setItem(ONBOARDING_STATUS_CACHE_KEY, String(user.has_completed_onboarding));
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
      const isUnconfirmed =
        isMekoApiRequestError(err) && err.code === API_ERROR_CODE_USER_NOT_CONFIRMED;
      setShowUnconfirmedRecovery(isUnconfirmed);
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
      if (
        isMekoApiRequestError(err) &&
        err.code === API_ERROR_CODE_USER_ALREADY_CONFIRMED
      ) {
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
    <div className="min-h-screen flex items-center justify-center bg-background px-4 text-foreground">
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-[#1C2333] p-8 shadow-sm">
        <h1 className="mb-6 text-center text-3xl font-semibold text-white">
          Sign in to Meko
        </h1>

        {showPasswordResetBanner ? (
          <div className="mb-4 rounded-lg border border-emerald-500/30 bg-emerald-950/25 px-4 py-3 text-sm text-emerald-100">
            Your password was updated. Sign in with your new password.
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
            className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
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
            className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
          />
          <Button
            type="submit"
            className="h-12 w-full bg-white text-black hover:bg-white/90"
            disabled={loading}
          >
            {loading ? "Please wait..." : "Continue"}
          </Button>
        </form>
        {error ? <p className="mt-4 text-sm text-red-600">{error}</p> : null}

        {showUnconfirmedRecovery ? (
          <div className="mt-5 rounded-lg border border-amber-500/25 bg-amber-950/20 px-4 py-3 text-sm text-amber-50/95">
            <p className="font-medium text-amber-100">Finish verifying this email</p>
            <p className="mt-1.5 text-xs leading-relaxed text-amber-100/80">
              Your account exists but the email is not verified yet.
              Resend the code to verify your email.
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
              <p className="mt-2 text-xs text-emerald-300/90">{resendMessage}</p>
            ) : null}
            {resendError ? <p className="mt-2 text-xs text-red-300">{resendError}</p> : null}
          </div>
        ) : null}

        <div className="mt-5 flex flex-col gap-2 text-center text-sm text-muted-foreground">
          <Link to={ROUTES.forgotPassword} className="font-medium text-blue-400 hover:underline">
            Forgot password?
          </Link>
          {/* <p>
            New to Meko?{" "}
            <Link to={ROUTES.signup} className="font-medium text-blue-400 hover:underline">
              Sign up
            </Link>
          </p> */}
          <Button variant="outline" className="h-11 w-full border-border bg-[#0E1525] text-white hover:bg-white/10" asChild>
            <a href={MEKO_REQUEST_ACCESS_URL} target="_blank" rel="noopener noreferrer">
              Request Access
            </a>
          </Button>
        </div>
      </div>
    </div>
  );
}
