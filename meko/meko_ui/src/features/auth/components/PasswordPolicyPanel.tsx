import { CheckCircle2, Circle, ShieldCheck } from "lucide-react";
import { cn } from "@/lib/utils";

type PasswordRequirement = {
  id: string;
  label: string;
  test: (password: string) => boolean;
  error: string;
};

const PASSWORD_REQUIREMENTS: PasswordRequirement[] = [
  {
    id: "length",
    label: "8 or more characters",
    test: (password) => password.length >= 8,
    error: "Password must be at least 8 characters long.",
  },
  {
    id: "lowercase",
    label: "One lowercase letter",
    test: (password) => /[a-z]/.test(password),
    error: "Password must include at least one lowercase letter.",
  },
  {
    id: "uppercase",
    label: "One uppercase letter",
    test: (password) => /[A-Z]/.test(password),
    error: "Password must include at least one uppercase letter.",
  },
  {
    id: "number",
    label: "One number",
    test: (password) => /[0-9]/.test(password),
    error: "Password must include at least one number.",
  },
  {
    id: "symbol",
    label: "One special character",
    test: (password) => /[!@#$%^&*()_+\-=[\]{};':",./<>?\\|`~]/.test(password),
    error: "Password must include at least one special character.",
  },
];

export function getPasswordPolicyError(password?: string | null): string | null {
  const pw = password || "";
  return PASSWORD_REQUIREMENTS.find((requirement) => !requirement.test(pw))?.error ?? null;
}

export function isPasswordPolicyMet(password?: string | null): boolean {
  return getPasswordPolicyError(password) === null;
}

function getPolicyState(password?: string | null) {
  const pw = password || "";
  const items = PASSWORD_REQUIREMENTS.map((requirement) => ({
    ...requirement,
    met: requirement.test(pw),
  }));
  const metCount = items.filter((item) => item.met).length;
  const percent = Math.round((metCount / PASSWORD_REQUIREMENTS.length) * 100);

  if (!pw) {
    return {
      items,
      metCount,
      percent,
      label: "Password rules",
      description: "Use all five requirements below.",
      tone: "bg-muted-foreground/30",
      textTone: "text-muted-foreground",
    };
  }

  if (metCount === PASSWORD_REQUIREMENTS.length) {
    return {
      items,
      metCount,
      percent,
      label: "Strong password",
      description: "Password requirements are met.",
      tone: "bg-emerald-500",
      textTone: "text-emerald-600 dark:text-emerald-400",
    };
  }

  if (metCount >= 3) {
    return {
      items,
      metCount,
      percent,
      label: "Almost there",
      description: `${PASSWORD_REQUIREMENTS.length - metCount} more requirement${
        PASSWORD_REQUIREMENTS.length - metCount === 1 ? "" : "s"
      } to go.`,
      tone: "bg-amber-500",
      textTone: "text-amber-600 dark:text-amber-400",
    };
  }

  return {
    items,
    metCount,
    percent,
    label: "Keep going",
    description: "Add more variety to meet the password policy.",
    tone: "bg-sky-500",
    textTone: "text-sky-600 dark:text-sky-400",
  };
}

type PasswordPolicyPanelProps = {
  password?: string | null;
  id?: string;
  className?: string;
};

export function PasswordPolicyPanel({
  password,
  id = "password-policy",
  className,
}: PasswordPolicyPanelProps) {
  const state = getPolicyState(password);

  return (
    <div
      id={id}
      className={cn("rounded-xl border border-border/80 bg-muted/30 p-4 shadow-sm", className)}
      aria-live="polite"
    >
      <div className="flex items-start gap-3">
        <div className="mt-0.5 rounded-full bg-primary/10 p-2 text-primary">
          <ShieldCheck className="h-4 w-4" aria-hidden />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center justify-between gap-3">
            <p className="text-sm font-medium text-foreground">{state.label}</p>
            <p className={cn("text-xs font-medium", state.textTone)}>
              {state.metCount}/{PASSWORD_REQUIREMENTS.length}
            </p>
          </div>
          <p className="mt-0.5 text-xs text-muted-foreground">{state.description}</p>
          <div
            className="mt-3 h-2 overflow-hidden rounded-full bg-muted"
            role="progressbar"
            aria-label="Password policy progress"
            aria-valuemin={0}
            aria-valuemax={PASSWORD_REQUIREMENTS.length}
            aria-valuenow={state.metCount}
          >
            <div
              className={cn("h-full rounded-full transition-all", state.tone)}
              style={{ width: `${state.percent}%` }}
            />
          </div>
        </div>
      </div>

      <ul className="mt-3 grid gap-2 text-xs sm:grid-cols-2">
        {state.items.map((item) => (
          <li
            key={item.id}
            className={cn(
              "flex items-center gap-2 rounded-lg px-2 py-1.5 transition-colors",
              item.met
                ? "bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
                : "text-muted-foreground",
            )}
          >
            {item.met ? (
              <CheckCircle2 className="h-3.5 w-3.5 shrink-0" aria-hidden />
            ) : (
              <Circle className="h-3.5 w-3.5 shrink-0" aria-hidden />
            )}
            <span>{item.label}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
