import { FormEvent, useEffect, useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
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

export type CompleteNewPasswordLocationState = {
  session: string;
  email: string;
};

/** Same rule as forgot-password flow: strictly more than 6 characters (minimum 7). */
const MIN_NEW_PASSWORD_LEN = 7;

function validateNewPasswords(newPassword: string, confirm: string): string | null {
  if (newPassword.length < MIN_NEW_PASSWORD_LEN) {
    return `Password must be more than 6 characters (at least ${MIN_NEW_PASSWORD_LEN}).`;
  }
  if (newPassword !== confirm) {
    return "Password and confirmation do not match.";
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

    const pwErr = validateNewPasswords(newPassword, confirmPassword);
    if (pwErr) {
      setError(pwErr);
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

  return (
    <div className="min-h-screen flex items-center justify-center bg-background px-4 text-foreground">
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-[#1C2333] p-8 shadow-sm">
        <h1 className="mb-2 text-center text-3xl font-semibold text-white">
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
            <Input
              id="cnp-password"
              type="password"
              placeholder="Choose a password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
              required
              minLength={MIN_NEW_PASSWORD_LEN}
              className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="cnp-confirm" className="text-muted-foreground">
              Confirm password
            </Label>
            <Input
              id="cnp-confirm"
              type="password"
              placeholder="Re-enter password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              autoComplete="new-password"
              required
              minLength={MIN_NEW_PASSWORD_LEN}
              className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
            />
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
                className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
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
                className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
              />
            </div>
          </div>
          <Button
            type="submit"
            className="h-12 w-full bg-white text-black hover:bg-white/90"
            disabled={loading}
          >
            {loading ? "Please wait..." : "Continue"}
          </Button>

          <p className="text-center text-xs text-gray-300">
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
    </div>
  );
}
