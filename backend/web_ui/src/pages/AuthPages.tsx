// The screens shown in place of the app: when local access didn't take a request as local, and
// when Meko didn't answer.

import type { ReactNode } from "react";
import { Button, Code, ErrorNote } from "../components/ui";

function AuthFrame({ title, sub, children }: { title: string; sub: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center px-4 py-8">
      <div className="w-full max-w-sm space-y-5 rounded-lg border bg-card p-6 shadow-sm">
        <img src="/meko-logo.svg" alt="Meko" className="h-7 dark:hidden" />
        <img src="/meko-logo-white.svg" alt="Meko" className="hidden h-7 dark:block" />
        <div className="space-y-1.5">
          <h1 className="text-lg font-semibold">{title}</h1>
          <div className="text-sm text-muted-foreground">{sub}</div>
        </div>
        {children}
      </div>
    </div>
  );
}

// Local access lets in only requests made on this computer straight to Meko; one that came through
// a proxy, or by another name, is refused.
export function NotLocalPage() {
  return (
    <AuthFrame
      title="This request wasn't recognized as local"
      sub="Meko is set to local access, which takes only requests made on the computer it runs on, straight to its address. This one came another way, for example through a proxy."
    >
      <p className="text-sm text-muted-foreground">
        On that computer, open Meko with <Code>meko ui</Code>. To reach it from other devices, set{" "}
        <Code>[web] access = "remote"</Code> in <Code>config.toml</Code> in Meko's data folder and restart Meko. Remote
        access has no login: anyone who can reach this computer's port can use Meko.
      </p>
    </AuthFrame>
  );
}

// The status couldn't be read: the server is down or isn't Meko.
export function UnreachablePage({ error, onRetry }: { error: Error; onRetry: () => void }) {
  return (
    <AuthFrame title="Meko didn't answer" sub="Check that Meko is running (meko status), then try again.">
      <ErrorNote error={error} />
      <Button onClick={onRetry}>Try again</Button>
    </AuthFrame>
  );
}
