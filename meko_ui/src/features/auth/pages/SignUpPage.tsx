import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { getApiErrorMessage, signup, confirmSignup } from "@/lib/api/mekoApi";
import { AUTH_METHODS, TELEMETRY_EVENTS } from "@/lib/telemetry/events";
import { identifyThenTrack } from "@/lib/telemetry/rudderstack";
import { Eye, EyeOff } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { ROUTES } from "@/app/routes/paths";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import {
  getPasswordPolicyError,
  PasswordPolicyPanel,
} from "@/features/auth/components/PasswordPolicyPanel";

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
      navigate(ROUTES.login, { replace: true });
    } catch (err) {
      setError(getApiErrorMessage(err, "Confirmation failed. Please try again."));
    } finally {
      setLoading(false);
    }
  };

  // Only warn once the user has started typing the confirmation, so the field
  // doesn't show an error while it's still empty.
  const confirmMismatch = confirmPassword.length > 0 && password !== confirmPassword;

  return (
    <AuthPageShell>
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggleButton />
      </div>
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-card p-8 shadow-sm">
        <h1 className="mb-6 text-center text-3xl font-semibold text-foreground">
          {awaitingCode ? "Confirm your account" : "Sign up for Meko"}
        </h1>
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
              By clicking &ldquo;Continue&rdquo;, you are creating a Meko account, and you agree to
              Yugabyte&apos;s{" "}
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
              <Link to={ROUTES.login} className="font-medium text-brand hover:underline">
                Sign in
              </Link>
            </p>
          </>
        ) : null}
      </div>
    </AuthPageShell>
  );
}
