import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { getApiErrorMessage, signup, confirmSignup } from "@/lib/api/mekoApi";
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
  const [code, setCode] = useState("");
  const [awaitingCode, setAwaitingCode] = useState(false);
  const [showPassword, setShowPassword] = useState(false);
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

    setLoading(true);
    setError(null);
    try {
      await signup({
        first_name: firstName.trim(),
        last_name: lastName.trim(),
        email: email.trim(),
        password,
      });
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
            disabled={loading}
          >
            {awaitingCode
              ? loading
                ? "Please wait..."
                : "Verify"
              : loading
                ? "Sending code..."
                : "Continue"}
          </Button>
        </form>

        {!awaitingCode ? (
          <>
            <p className="mt-5 text-center text-sm text-muted-foreground">
              Already have an account?{" "}
              <Link to={ROUTES.login} className="font-medium text-blue-400">
                Sign in
              </Link>
            </p>
          </>
        ) : null}
      </div>
    </AuthPageShell>
  );
}
