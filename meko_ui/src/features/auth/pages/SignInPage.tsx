import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { login } from "@/lib/mekoApi";
import { ROUTES } from "@/app/routes/paths";

export default function SignInPage() {
  const navigate = useNavigate();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLoading(true);
    setError(null);
    try {
      await login({ email: email.trim(), password });
      navigate(ROUTES.datapacks, { replace: true });
    } catch (err) {
      setError(err instanceof Error ? err.message : "signin failed");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-background px-4 text-foreground">
      <div className="w-full max-w-[560px] rounded-2xl border border-border bg-[#1C2333] p-8 shadow-sm">
        <h1 className="mb-6 text-center text-3xl font-semibold text-white">
          Sign in to Meko
        </h1>

        <form className="space-y-4" onSubmit={handleSubmit}>
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
        <p className="mt-5 text-center text-sm text-muted-foreground">
          New to Meko?{" "}
          <Link to={ROUTES.signup} className="font-medium text-blue-400">
            Sign up
          </Link>
        </p>
      </div>
    </div>
  );
}
