import { FormEvent, useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { getApiErrorMessage, signup, confirmSignup } from "@/lib/mekoApi";
import { Input } from "@/components/ui/input";
import { Button } from "@/components/ui/button";
import { ROUTES } from "@/app/routes/paths";

function getPasswordPolicyError(password: string): string | null {
  if (password.length < 8) return "Password must be at least 8 characters long.";
  if (!/[a-z]/.test(password))
    return "Password must include at least one lowercase letter.";
  if (!/[A-Z]/.test(password))
    return "Password must include at least one uppercase letter.";
  if (!/[0-9]/.test(password))
    return "Password must include at least one number.";
  if (!/[^A-Za-z0-9]/.test(password))
    return "Password must include at least one special character.";
  return null;
}

export default function SignUpPage() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [firstName, setFirstName] = useState("");
  const [lastName, setLastName] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [awaitingCode, setAwaitingCode] = useState(false);
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
    <div className="min-h-screen flex items-center justify-center bg-background px-4 text-foreground">
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-[#1C2333] p-8 shadow-sm">
        <h1 className="mb-6 text-center text-3xl font-semibold text-white">
          {awaitingCode ? "Confirm your account" : "Sign up for Meko"}
        </h1>
        <form
          className="space-y-4"
          onSubmit={awaitingCode ? handleConfirm : handleSignup}
        >
          {!awaitingCode ? (
            <>
              <div className="grid grid-cols-2 gap-3">
                <Input
                  placeholder="Your first name"
                  value={firstName}
                  onChange={(e) => setFirstName(e.target.value)}
                  required
                  className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
                />
                <Input
                  placeholder="Your last name"
                  value={lastName}
                  onChange={(e) => setLastName(e.target.value)}
                  required
                  className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
                />
              </div>
              <Input
                type="email"
                placeholder="Your email address"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                required
                className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
              />
              <Input
                type="password"
                placeholder="Create password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                required
                className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
              />
              <p className="text-xs text-muted-foreground">
                Use 8+ chars with uppercase, lowercase, number, and special
                character.
              </p>
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
                className="h-12 border-border bg-[#0E1525] text-white placeholder:text-muted-foreground"
              />
            </>
          )}

          {error ? <p className="text-sm text-red-600">{error}</p> : null}

          <Button
            type="submit"
            className="h-12 w-full bg-white text-black hover:bg-white/90"
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
    </div>
  );
}
