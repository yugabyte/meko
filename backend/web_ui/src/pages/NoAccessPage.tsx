// Shown when the browser has no working token: there's no login, so it says how to open the UI.

import { KeyRound } from "lucide-react";
import { CopyBlock } from "../components/CopyBlock";

export function NoAccessPage() {
  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <div className="w-full max-w-md space-y-4 rounded-lg border bg-card p-6">
        <div className="flex items-center gap-2">
          <KeyRound className="size-5 text-primary" />
          <h1 className="font-semibold">Open Meko from the terminal</h1>
        </div>
        <p className="text-sm text-muted-foreground">
          Meko has no login. Run this on the machine Meko runs on, and it opens this page with your token, which the
          browser then keeps:
        </p>
        <CopyBlock text="meko ui" />
        <p className="text-xs text-muted-foreground">
          The token changed? Run it again. <code>meko ui --no-open</code> only prints the link.
        </p>
      </div>
    </div>
  );
}
