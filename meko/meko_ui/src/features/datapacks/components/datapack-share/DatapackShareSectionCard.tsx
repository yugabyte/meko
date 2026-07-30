import type { ReactNode } from "react";
import { Card, CardContent, CardHeader } from "@/components/ui/card";
import { cn } from "@/lib/utils";

type Props = {
  title: string;
  icon?: ReactNode;
  /** Extra label on the right of the title row (badges, actions). */
  trailing?: ReactNode;
  children: ReactNode;
  className?: string;
  /** e.g. destructive border for transfer ownership */
  tone?: "default" | "danger";
};

export function DatapackShareSectionCard({
  title,
  icon,
  trailing,
  children,
  className,
  tone = "default",
}: Props) {
  return (
    <Card
      className={cn(
        "border-border bg-card/50 shadow-sm",
        tone === "danger" && "border-destructive/40 bg-destructive/[0.06]",
        className,
      )}
    >
      <CardHeader className="flex flex-row items-center justify-between gap-3 space-y-0 pb-4">
        <div className="flex min-w-0 items-center gap-2">
          {icon ? <span className="shrink-0 text-muted-foreground [&_svg]:h-5 [&_svg]:w-5">{icon}</span> : null}
          <h2 className="text-base font-semibold tracking-tight text-foreground">{title}</h2>
        </div>
        {trailing ? <div className="shrink-0">{trailing}</div> : null}
      </CardHeader>
      <CardContent className="pt-0">{children}</CardContent>
    </Card>
  );
}
