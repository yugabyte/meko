// MetricCard — a single admin dashboard stat tile: title, big accent-coloured
// value (compacted to K/M), an icon, and an optional description line.
import { type LucideIcon } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

type MetricCardProps = {
  title: string;
  value: number | undefined;
  icon: LucideIcon;
  description?: string;
  isLoading?: boolean;
  className?: string;
  // Tailwind text-color class (e.g. "text-orange-400") applied to the big
  // value and the icon so the grid reads as a tasteful multi-color set.
  accentClassName?: string;
};

function formatNumber(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return n.toLocaleString();
}

export function MetricCard({
  title,
  value,
  icon: Icon,
  description,
  isLoading,
  className,
  accentClassName,
}: MetricCardProps) {
  return (
    <Card className={cn("", className)}>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium text-muted-foreground">{title}</CardTitle>
        <Icon className={cn("h-4 w-4", accentClassName ?? "text-muted-foreground")} />
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-8 w-24" />
        ) : (
          <p className={cn("text-2xl font-bold", accentClassName)}>
            {value !== undefined ? formatNumber(value) : "—"}
          </p>
        )}
        {description && <p className="text-xs text-muted-foreground mt-1">{description}</p>}
      </CardContent>
    </Card>
  );
}
