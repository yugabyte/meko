import { FormEvent, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  forgotPasswordConfirm,
  forgotPasswordSendCode,
  getApiErrorMessage,
} from "@/lib/mekoApi";
import { ROUTES } from "@/app/routes/paths";
import { AuthPageShell } from "@/features/auth/components/AuthPageShell";
import { ThemeToggleButton } from "@/features/layout/ThemeToggleButton";

/** Matches HTML5-style email; server still validates. */
function isValidEmailFormat(value: string): boolean {
  const v = value.trim();
  if (!v) return false;
  return /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(v);
}

/** Same rule as API: strictly more than 6 characters (minimum 7). */
const MIN_NEW_PASSWORD_LEN = 7;

function validateNewPasswords(
  newPassword: string,
  confirm: string,
): string | null {
  if (newPassword.length < MIN_NEW_PASSWORD_LEN) {
    return `Password must be more than 6 characters (at least ${MIN_NEW_PASSWORD_LEN}).`;
  }
  if (newPassword !== confirm) {
    return "New password and confirmation do not match.";
  }
  return null;
}

/**
 * Self-service password reset: Cognito ForgotPassword emails a code;
 * ConfirmForgotPassword applies the new password. UX matches SignIn/SignUp cards.
 */
export default function ForgotPasswordPage() {
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [sendLoading, setSendLoading] = useState(false);
  const [sendError, setSendError] = useState<string | null>(null);
  /** True only after Cognito accepted ForgotPassword for this email. */
  const [codeSent, setCodeSent] = useState(false);
  /** Reveals code + password fields after user clicks “Show activation code”. */
  const [showActivationSection, setShowActivationSection] = useState(false);
  const [code, setCode] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [updateLoading, setUpdateLoading] = useState(false);
  const [updateError, setUpdateError] = useState<string | null>(null);

  const handleSendCode = async (e: FormEvent) => {
    e.preventDefault();
    setSendError(null);
    setCodeSent(false);
    setShowActivationSection(false);
    const trimmed = email.trim();
    if (!isValidEmailFormat(trimmed)) {
      setSendError("Enter a valid email address.");
      return;
    }
    setSendLoading(true);
    try {
      await forgotPasswordSendCode({ email: trimmed });
      // 200 means Cognito accepted ForgotPassword (pool usually returns 404 if username unknown).
      setCodeSent(true);
    } catch (err) {
      setSendError(getApiErrorMessage(err, "Could not send code. Try again."));
    } finally {
      setSendLoading(false);
    }
  };

  const handleUpdatePassword = async (e: FormEvent) => {
    e.preventDefault();
    setUpdateError(null);
    const pwErr = validateNewPasswords(newPassword, confirmPassword);
    if (pwErr) {
      setUpdateError(pwErr);
      return;
    }
    setUpdateLoading(true);
    try {
      await forgotPasswordConfirm({
        email: email.trim(),
        code: code.trim(),
        new_password: newPassword,
        confirm_password: confirmPassword,
      });
      navigate(`${ROUTES.signin}?reset=success`, { replace: true });
    } catch (err) {
      setUpdateError(getApiErrorMessage(err, "Could not update password. Try again."));
    } finally {
      setUpdateLoading(false);
    }
  };

  return (
    <AuthPageShell>
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggleButton />
      </div>
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-card p-8 shadow-sm">
        <h1 className="mb-2 text-center text-3xl font-semibold text-foreground">Forgot password</h1>
        <p className="mb-6 text-center text-sm text-muted-foreground">
          We&apos;ll email you a code to reset your password.
        </p>

        <form className="space-y-4" onSubmit={handleSendCode}>
          <Input
            type="email"
            autoComplete="username"
            name="email"
            aria-label="Email address"
            inputMode="email"
            autoCapitalize="none"
            spellCheck={false}
            enterKeyHint="send"
            placeholder="Your email address"
            value={email}
            onChange={(e) => {
              setEmail(e.target.value);
              if (codeSent) {
                setCodeSent(false);
                setShowActivationSection(false);
              }
            }}
            required
            className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
          />
          <Button
            type="submit"
            className="h-12 w-full bg-primary text-primary-foreground hover:bg-primary/90"
            disabled={sendLoading}
          >
            {sendLoading ? "Sending…" : "Send activation code"}
          </Button>
        </form>

        {sendError ? <p className="mt-3 text-sm text-red-600">{sendError}</p> : null}
        {codeSent && !sendError ? (
          <p className="mt-3 text-sm text-emerald-400/90">
            If an account exists for this email, a verification code has been sent.
          </p>
        ) : null}

        <div className="mt-6 border-t border-border pt-6">
          <Button
            type="button"
            variant="secondary"
            className="h-10 w-full bg-white/10 text-white hover:bg-white/15"
            disabled={!codeSent}
            onClick={() => setShowActivationSection(true)}
          >
            Show activation code
          </Button>
          <p className="mt-2 text-center text-xs text-muted-foreground">
            Enabled after a code is sent to an account on file for this email.
          </p>
        </div>

        {showActivationSection && codeSent ? (
          <form className="mt-6 space-y-4 border-t border-border pt-6" onSubmit={handleUpdatePassword}>
            <p className="text-sm text-muted-foreground">
              Enter the code from your email, then choose a new password.
            </p>
            <Input
              placeholder="Verification code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              required
              autoComplete="one-time-code"
              className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
            />
            <Input
              type="password"
              placeholder="Enter new password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              required
              autoComplete="new-password"
              className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
            />
            <Input
              type="password"
              placeholder="Confirm new password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              required
              autoComplete="new-password"
              className="h-12 border-border bg-input text-foreground placeholder:text-muted-foreground"
            />
            <p className="text-xs text-muted-foreground">
              Password must be more than 6 characters. Both fields must match.
            </p>
            {updateError ? <p className="text-sm text-red-600">{updateError}</p> : null}
            <Button
              type="submit"
              className="h-12 w-full bg-primary text-primary-foreground hover:bg-primary/90"
              disabled={updateLoading}
            >
              {updateLoading ? "Updating…" : "Update password"}
            </Button>
          </form>
        ) : null}

        <p className="mt-6 text-center text-sm text-muted-foreground">
          <Link to={ROUTES.signin} className="font-medium text-blue-400">
            Back to sign in
          </Link>
        </p>
      </div>
    </AuthPageShell>
  );
}
