// Account-mode picker: 2 cards, modeled on api_server's operator console
// (console.html's "Onboarding mode" attach/provision selector) translated to
// this feature's React/shadcn conventions.

import { Badge } from "@/components/ui/badge";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";

export type AccountMode = "managed" | "byoc";

const ACCOUNT_MODES: { id: AccountMode; tag: string; title: string; description: string }[] = [
  {
    id: "managed",
    tag: "Fastest",
    title: "Meko's managed AWS account",
    description: "Meko runs and owns the deployment; nothing to configure in your own AWS account.",
  },
  {
    id: "byoc",
    tag: "Your AWS account",
    title: "Your own AWS account (BYOC)",
    description: "Deploys into an IAM role you create; Meko never holds long-lived credentials in your account.",
  },
];

export function AccountModePicker({
  selected,
  onSelect,
}: {
  selected: AccountMode | null;
  onSelect: (mode: AccountMode) => void;
}) {
  return (
    <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
      {ACCOUNT_MODES.map((mode) => (
        <Card
          key={mode.id}
          role="button"
          tabIndex={0}
          onClick={() => onSelect(mode.id)}
          onKeyDown={(e) => {
            if (e.key === "Enter" || e.key === " ") onSelect(mode.id);
          }}
          className={cn(
            "cursor-pointer p-4 transition-colors hover:bg-muted/30",
            selected === mode.id && "border-primary ring-2 ring-primary/30",
          )}
        >
          <div className="flex items-center justify-between gap-2">
            <p className="text-sm font-medium">{mode.title}</p>
            <Badge variant="secondary">{mode.tag}</Badge>
          </div>
          <p className="mt-1 text-xs text-muted-foreground">{mode.description}</p>
        </Card>
      ))}
    </div>
  );
}
