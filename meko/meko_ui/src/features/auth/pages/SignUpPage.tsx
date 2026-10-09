import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { getApiErrorMessage, signup, confirmSignup } from "@/lib/api/mekoApi";
import { AUTH_METHODS, TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { identifyThenTrack } from "@/lib/telemetry/rudderstack";
import { Eye, EyeOff, Network, Share2, ScanLine, Gauge } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { ROUTES } from "@/app/routes/paths";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import {
  getPasswordPolicyError,
  PasswordPolicyPanel,
} from "@/features/auth/components/PasswordPolicyPanel";
import { safeReturnTo } from "@/features/auth/returnTo";

// Marketing pitch shown alongside the sign-up form. Copy is owned by
// the marketing team; keep this list in sync with the "Sign up for
// Meko" mock in the brand deck. Icon per bullet so the four benefits
// scan quickly.
const SIGNUP_HIGHLIGHTS: ReadonlyArray<{
  icon: React.ComponentType<{ className?: string; "aria-hidden"?: boolean }>;
  text: string;
}> = [
  { icon: Network, text: "Share context instantly across fleets of agents" },
  { icon: Share2, text: "Share datapacks with any agent or human" },
  { icon: ScanLine, text: "Spot agent issues fast with decision traces" },
  { icon: Gauge, text: "Cut token burn by up to 45%" },
];

export default function SignUpPage() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [firstName, setFirstName] = useState("");
  const [lastName, setLastName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [code, setCode] = useState("");
  const [awaitingCode, setAwaitingCode] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
  const [showConfirmPassword, setShowConfirmPassword] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);

  // Deep-link from sign-in when the user must finish Cognito email verification:
  // `/signup?resume=1&email=...` opens the confirmation-code step with email prefilled.
  useEffect(() => {
    const resume = searchParams.get("resume");
    const qEmail = searchParams.get("email");
    if (resume === "1" && qEmail?.trim()) {
      setEmail(qEmail.trim());
      setAwaitingCode(true);
      setError(null);
    }
  }, [searchParams]);

  const handleSignup = async (e: FormEvent) => {
    e.preventDefault();
    const passwordErr = getPasswordPolicyError(password);
    if (passwordErr) {
      setError(passwordErr);
      return;
    }
    if (password !== confirmPassword) {
      setError("Passwords don't match.");
      return;
    }

    setLoading(true);
    setError(null);
    try {
      await signup({
        first_name: firstName.trim(),
        last_name: lastName.trim(),
        email: email.trim(),
        password,
      });
      // Identify FIRST (by email — no account_id exists until confirmation) so the
      // HubSpot contact is created, then fire user_signed_up after a short gap so
      // it maps to that contact.
      identifyThenTrack(
        {
          email: email.trim(),
          firstName: firstName.trim(),
          lastName: lastName.trim(),
        },
        TELEMETRY_EVENTS.USER_SIGNED_UP,
        {
          email: email.trim(),
          signup_method: AUTH_METHODS.EMAIL_PASSWORD,
        },
      );
      setAwaitingCode(true);
    } catch (err) {
      setError(getApiErrorMessage(err, "Sign up failed. Please try again."));
    } finally {
      setLoading(false);
    }
  };

  const handleConfirm = async (e: FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError(null);
    try {
      await confirmSignup({ email: email.trim(), code: code.trim() });
      // Carry return_to through to the sign-in page so post-signup
      // flows (AWS Marketplace complete-registration, invite links)
      // resume after auth. Navigate to ROUTES.signin directly rather
      // than ROUTES.login because PublicRoutes' `/login` -> `/signin`
      // <Navigate> redirect drops the query string on the way.
      const returnTo = safeReturnTo(searchParams.get("return_to"));
      const signinPath = returnTo
        ? `${ROUTES.signin}?return_to=${encodeURIComponent(returnTo)}`
        : ROUTES.signin;
      navigate(signinPath, { replace: true });
    } catch (err) {
      setError(getApiErrorMessage(err, "Confirmation failed. Please try again."));
    } finally {
      setLoading(false);
    }
  };

  // Only warn once the user has started typing the confirmation, so the field
  // doesn't show an error while it's still empty.
  const confirmMismatch = confirmPassword.length > 0 && password !== confirmPassword;

  // Layout:
  //   - Initial sign-up step: two-column (marketing left, form right)
  //     from `lg` up; stacked on md / mobile with marketing above the
  //     form so first-visit visitors read the pitch before scrolling.
  //   - Confirmation code step: single-column card (existing width) so
  //     the code entry stays the sole focus — the marketing pitch is
  //     noise once someone has already committed to signing up.
  const outerCls = awaitingCode
    ? "w-full flex justify-center"
    : "grid w-full max-w-6xl grid-cols-1 items-center gap-8 lg:grid-cols-[minmax(0,1fr)_minmax(0,560px)] lg:gap-12";

  return (
    <AuthPageShell>
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggleButton />
      </div>
      <div className={outerCls}>
        {!awaitingCode && <SignUpMarketing />}
        <div className="w-full max-w-[560px] justify-self-center rounded-2xl border border-border bg-card p-8 shadow-sm lg:justify-self-end">
          <h1 className="mb-6 text-center text-3xl font-semibold text-foreground">
            {awaitingCode ? "Confirm your account" : "Sign up for Meko"}
          </h1>
          {searchParams.get("source") === "aws-marketplace" && !awaitingCode ? (
            <div className="mb-4 rounded-lg border border-blue-500/30 bg-blue-500/[0.06] px-3 py-2 text-xs text-blue-700 dark:text-blue-300">
              You're finishing your AWS Marketplace subscription. Create your Meko account to unlock
              your Standard plan.
            </div>
          ) : null}
          <form className="space-y-4" onSubmit={awaitingCode ? handleConfirm : handleSignup}>
            {!awaitingCode ? (
              <>
                <div className="grid grid-cols-2 gap-3">
                  <Input
                    placeholder="Your first name"
                    value={firstName}
                    onChange={(e) => setFirstName(e.target.value)}
                    required
                    className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
                  />
                  <Input
                    placeholder="Your last name"
                    value={lastName}
                    onChange={(e) => setLastName(e.target.value)}
                    required
                    className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
                  />
                </div>
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
                  onChange={(e) => setEmail(e.target.value)}
                  required
                  className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
                />
                <div className="space-y-3">
                  <div className="relative">
                    <Input
                      id="signup-password"
                      type={showPassword ? "text" : "password"}
                      autoComplete="new-password"
                      name="password"
                      aria-label="Password"
                      aria-describedby="signup-password-policy"
                      enterKeyHint="go"
                      placeholder="Create password"
                      value={password}
                      onChange={(e) => setPassword(e.target.value)}
                      required
                      className="h-12 border-border bg-input pr-12 text-foreground placeholder:text-muted-foreground"
                    />
                    <button
                      type="button"
                      aria-label={showPassword ? "Hide password" : "Show password"}
                      aria-pressed={showPassword}
                      className="absolute inset-y-0 right-0 flex w-12 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                      onClick={() => setShowPassword((value) => !value)}
                    >
                      {showPassword ? (
                        <EyeOff className="h-4 w-4" aria-hidden />
                      ) : (
                        <Eye className="h-4 w-4" aria-hidden />
                      )}
                    </button>
                  </div>
                  <div className="relative">
                    <Input
                      id="signup-confirm-password"
                      type={showConfirmPassword ? "text" : "password"}
                      autoComplete="new-password"
                      name="confirmPassword"
                      aria-label="Confirm password"
                      aria-invalid={confirmMismatch}
                      aria-describedby={confirmMismatch ? "signup-confirm-error" : undefined}
                      enterKeyHint="go"
                      placeholder="Confirm password"
                      value={confirmPassword}
                      onChange={(e) => setConfirmPassword(e.target.value)}
                      required
                      className="h-12 border-border bg-input pr-12 text-foreground placeholder:text-muted-foreground"
                    />
                    <button
                      type="button"
                      aria-label={showConfirmPassword ? "Hide password" : "Show password"}
                      aria-pressed={showConfirmPassword}
                      className="absolute inset-y-0 right-0 flex w-12 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                      onClick={() => setShowConfirmPassword((value) => !value)}
                    >
                      {showConfirmPassword ? (
                        <EyeOff className="h-4 w-4" aria-hidden />
                      ) : (
                        <Eye className="h-4 w-4" aria-hidden />
                      )}
                    </button>
                  </div>
                  {confirmMismatch ? (
                    <p id="signup-confirm-error" className="text-sm text-red-600">
                      Passwords don't match.
                    </p>
                  ) : null}
                  <PasswordPolicyPanel id="signup-password-policy" password={password} />
                </div>
              </>
            ) : (
              <>
                <p className="text-sm text-muted-foreground">
                  Enter the confirmation code sent to <b>{email}</b>.
                </p>
                <Input
                  placeholder="6-digit confirmation code"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  required
                  className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
                />
              </>
            )}

            {error ? <p className="text-sm text-red-600">{error}</p> : null}

            <Button
              type="submit"
              className="h-12 w-full bg-primary text-primary-foreground hover:bg-primary/90"
              disabled={loading || (!awaitingCode && confirmMismatch)}
            >
              {awaitingCode
                ? loading
                  ? "Please wait..."
                  : "Verify"
                : loading
                  ? "Sending code..."
                  : "Continue"}
            </Button>

            {!awaitingCode ? (
              <p className="text-center text-xs text-muted-foreground">
                By clicking &ldquo;Continue&rdquo;, you are creating a Meko account, and you agree
                to Yugabyte&apos;s{" "}
                <a
                  href="https://mekodata.ai/early-access-terms-of-service/"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="underline hover:text-foreground"
                >
                  Terms of Use
                </a>
                .
              </p>
            ) : null}
          </form>

          {!awaitingCode ? (
            <>
              <p className="mt-5 text-center text-sm text-muted-foreground">
                Already have an account?{" "}
                <Link
                  to={
                    // Carry return_to through so an existing AWS Marketplace
                    // buyer who clicks "Sign in instead" still lands on
                    // /marketplace/complete after auth. Uses ROUTES.signin
                    // (not ROUTES.login) because /login → <Navigate to=/signin>
                    // strips the query string.
                    (() => {
                      const rt = safeReturnTo(searchParams.get("return_to"));
                      return rt
                        ? `${ROUTES.signin}?return_to=${encodeURIComponent(rt)}`
                        : ROUTES.signin;
                    })()
                  }
                  className="font-medium text-brand hover:underline"
                >
                  Sign in
                </Link>
              </p>
            </>
          ) : null}
        </div>
      </div>
    </AuthPageShell>
  );
}

// Marketing pane rendered next to the sign-up form on the initial step.
// Kept in-file (rather than a shared component) because this copy is
// specific to Sign up — the marketing team calibrates it per-page
// (Sign in stays clean; Reset password stays clean).
function SignUpMarketing() {
  return (
    <section className="max-w-xl px-1 lg:px-0">
      <h2 className="text-3xl font-semibold leading-tight text-foreground sm:text-4xl">
        Shared context, delivered as infrastructure
      </h2>
      <p className="mt-3 text-base italic text-muted-foreground">
        Meko is an agent-native data infrastructure for collective memory, shared knowledge, and
        decision traces.
      </p>
      <p className="mt-6 text-sm font-medium text-foreground">
        Switch Meko on and, in less than ten minutes:
      </p>
      <ul className="mt-3 space-y-2.5">
        {SIGNUP_HIGHLIGHTS.map((h) => (
          <li key={h.text} className="flex items-start gap-3">
            {/* Circular icon chip picks up the brand tint so the four
                bullets read as a set. Small enough not to compete with
                the form on the right at desktop widths. */}
            <span className="mt-0.5 inline-flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-brand/12 text-brand ring-1 ring-brand/25">
              <h.icon className="h-3.5 w-3.5" aria-hidden />
            </span>
            <span className="text-sm leading-6 text-foreground">{h.text}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
