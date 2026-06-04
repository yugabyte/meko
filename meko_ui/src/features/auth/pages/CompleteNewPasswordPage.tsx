import { FormEvent, useEffect, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { Eye, EyeOff } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { toast } from "@/hooks/use-toast";
import {
  completeNewPassword,
  getApiErrorMessage,
  logout,
  PROVISION_STATUS_CACHE_KEY,
  ONBOARDING_STATUS_CACHE_KEY,
  AUTH_LOGOUT_EVENT
} from "@/lib/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { ROUTES } from "@/app/routes/paths";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import {
  getPasswordPolicyError,
  PasswordPolicyPanel,
} from "@/features/auth/components/PasswordPolicyPanel";

const passwordMismatchMessage = "Password and confirmation do not match.";

export type CompleteNewPasswordLocationState = {
  session: string;
  email: string;
};

function validateNewPasswords(newPassword: string, confirm: string): string | null {
  const policyError = getPasswordPolicyError(newPassword);
  if (policyError) return policyError;
  if (newPassword !== confirm) {
    return passwordMismatchMessage;
  }
  return null;
}

/**
 * Completes Cognito NEW_PASSWORD_REQUIRED after sign-in returns a challenge.
 * Keeps session server-side only (passed via router location state, not the URL).
 */
export default function CompleteNewPasswordPage() {
  const navigate = useNavigate();
  const location = useLocation();
  const state = location.state as CompleteNewPasswordLocationState | null;

  const [firstName, setFirstName] = useState("");
  const [lastName, setLastName] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [confirmPasswordTouched, setConfirmPasswordTouched] = useState(false);
  const [showPasswords, setShowPasswords] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!state?.session?.trim() || !state?.email?.trim()) {
      navigate(ROUTES.signin, { replace: true });
    }
  }, [state, navigate]);

  if (!state?.session?.trim() || !state?.email?.trim()) {
    return null;
  }

  const email = state.email.trim();
  const session = state.session;

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);

    const fn = firstName.trim();
    const ln = lastName.trim();
    if (!fn || !ln) {
      setError("First name and last name are required.");
      return;
    }

    setConfirmPasswordTouched(true);
    const pwErr = validateNewPasswords(newPassword, confirmPassword);
    if (pwErr) {
      setError(pwErr === passwordMismatchMessage ? null : pwErr);
      return;
    }

    setLoading(true);
    try {
      await completeNewPassword({
        email,
        session,
        new_password: newPassword,
        confirm_password: confirmPassword,
        first_name: fn,
        last_name: ln,
      });
      toast({
        title: "Account setup complete",
      });
      try {
        await logout();
        sessionStorage.removeItem(PROVISION_STATUS_CACHE_KEY);
        localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
        dispatchAppEvent(AUTH_LOGOUT_EVENT);
      } catch {
        // Still land on sign-in; user can retry sign-in if cookies were not cleared.
      }
      navigate(ROUTES.signin, { replace: true });
    } catch (err) {
      setError(getApiErrorMessage(err, "Could not finish account setup. Try again."));
    } finally {
      setLoading(false);
    }
  };

  const confirmPasswordMismatch =
    confirmPasswordTouched &&
    confirmPassword.length > 0 &&
    newPassword !== confirmPassword;

  return (
    <AuthPageShell>
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggleButton />
      </div>
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-card p-8 shadow-sm">
        <h1 className="mb-2 text-center text-3xl font-semibold text-foreground">
          Complete account setup
        </h1>
        <p className="mb-6 text-center text-sm text-muted-foreground">
          {email}
        </p>

        <form className="space-y-4" onSubmit={(ev) => void handleSubmit(ev)}>
          <div className="space-y-2">
            <Label htmlFor="cnp-password" className="text-muted-foreground">
              Password
            </Label>
            <div className="relative">
              <Input
                id="cnp-password"
                type={showPasswords ? "text" : "password"}
                placeholder="Choose a password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
                autoComplete="new-password"
                aria-describedby="complete-password-policy"
                required
                className="h-12 border-border bg-input pr-12 text-foreground placeholder:text-muted-foreground"
              />
              <button
                type="button"
                aria-label={showPasswords ? "Hide passwords" : "Show passwords"}
                aria-pressed={showPasswords}
                className="absolute inset-y-0 right-0 flex w-12 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                onClick={() => setShowPasswords((value) => !value)}
              >
                {showPasswords ? (
                  <EyeOff className="h-4 w-4" aria-hidden />
                ) : (
                  <Eye className="h-4 w-4" aria-hidden />
                )}
              </button>
            </div>
            <PasswordPolicyPanel
              id="complete-password-policy"
              password={newPassword}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="cnp-confirm" className="text-muted-foreground">
              Confirm password
            </Label>
            <Input
              id="cnp-confirm"
              type={showPasswords ? "text" : "password"}
              placeholder="Re-enter password"
              value={confirmPassword}
              onChange={(e) => {
                setConfirmPassword(e.target.value);
                if (!confirmPasswordTouched) setConfirmPasswordTouched(true);
              }}
              onBlur={() => setConfirmPasswordTouched(true)}
              autoComplete="new-password"
              aria-describedby={
                confirmPasswordMismatch
                  ? "cnp-confirm-help cnp-confirm-error"
                  : "cnp-confirm-help"
              }
              aria-invalid={confirmPasswordMismatch}
              required
              className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
            />
            <p id="cnp-confirm-help" className="text-xs text-muted-foreground">
              Re-enter your new password. It must match exactly.
            </p>
            {confirmPasswordMismatch ? (
              <p
                id="cnp-confirm-error"
                className="text-xs text-red-600 dark:text-red-300"
              >
                {passwordMismatchMessage}
              </p>
            ) : null}
          </div>
          <div className="grid grid-cols-2 gap-3">
            <div className="space-y-2">
              <Label htmlFor="cnp-first" className="text-muted-foreground">
                First name
              </Label>
              <Input
                id="cnp-first"
                placeholder="First name"
                value={firstName}
                onChange={(e) => setFirstName(e.target.value)}
                autoComplete="given-name"
                required
                className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="cnp-last" className="text-muted-foreground">
                Last name
              </Label>
              <Input
                id="cnp-last"
                placeholder="Last name"
                value={lastName}
                onChange={(e) => setLastName(e.target.value)}
                autoComplete="family-name"
                required
                className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
              />
            </div>
          </div>
          <Button
            type="submit"
            className="h-12 w-full bg-primary text-primary-foreground hover:bg-primary/90"
            disabled={loading}
          >
            {loading ? "Please wait..." : "Continue"}
          </Button>

          <p className="text-center text-xs text-muted-foreground">
            By clicking &ldquo;Continue&rdquo;, you are creating a Meko account, and you agree to
            Yugabyte&apos;s{" "}
            <a
              href="https://mekodata.ai/early-access-terms-of-service/"
              target="_blank"
              rel="noopener noreferrer"
              className="underline hover:text-white"
            >
              Terms of Use
            </a>
            .
          </p>
        </form>

        {error ? <p className="mt-4 text-sm text-red-600">{error}</p> : null}

        <p className="mt-5 text-center text-sm text-muted-foreground">
          <Link to={ROUTES.signin} className="font-medium text-blue-400 hover:underline">
            Back to sign in
          </Link>
        </p>
      </div>
    </AuthPageShell>
  );
}
