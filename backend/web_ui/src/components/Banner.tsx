// A notice across the top of a page: an icon, what to know, and optionally a command to copy, a
// link or button, and a close button. Warning for something to fix, primary for something to know.

import type { ReactNode } from "react";
import { X } from "lucide-react";
import { CopyBlock } from "./CopyBlock";
import { Button, cx } from "./ui";

const TONES = {
  warning: { box: "border-warning/40 bg-warning/10", icon: "text-warning" },
  primary: { box: "border-primary/40 bg-primary/10", icon: "text-primary" },
};

export function Banner({
  tone = "primary",
  icon,
  children,
  command,
  action,
  onClose,
  className,
}: {
  tone?: keyof typeof TONES;
  icon: ReactNode;
  children: ReactNode;
  command?: string;
  action?: ReactNode;
  onClose?: () => void;
  className?: string;
}) {
  const t = TONES[tone];
  return (
    <div className={cx("space-y-3 rounded-lg border px-4 py-3 text-sm", t.box, className)}>
      <div className="flex flex-wrap items-center gap-3">
        <span className={cx("shrink-0 [&>svg]:size-4", t.icon)}>{icon}</span>
        <div className="min-w-0 flex-1">{children}</div>
        {action}
        {onClose && (
          <Button size="sm" variant="ghost" onClick={onClose} aria-label="Dismiss">
            <X className="size-3.5" />
          </Button>
        )}
      </div>
      {command && <CopyBlock text={command} />}
    </div>
  );
}
