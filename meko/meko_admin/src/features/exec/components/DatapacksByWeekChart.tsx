// Week-over-week datapacks created across the selected range. Vertical bars,
// one per ISO week (Monday-start), gap-filled so empty weeks show as zero — the
// axis scales with whatever range is picked.
import { BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip, ResponsiveContainer } from "recharts";
import { Layers } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { CHART_ORANGE } from "@/features/dashboard/components/chartColors";
import type { ExecWeekCount } from "@/lib/adminApi";

type Props = {
  data: ExecWeekCount[] | undefined;
  start: Date | undefined;
  end: Date | undefined;
  isLoading: boolean;
};

// weekLabel formats a yyyy-MM-dd week_start (a UTC Monday from the backend) for
// the axis, reading the date in UTC so a viewer behind UTC doesn't see it shift
// to the previous day.
function weekLabel(key: string): string {
  return new Date(`${key}T00:00:00Z`).toLocaleDateString("en-US", {
    month: "short",
    day: "numeric",
    timeZone: "UTC",
  });
}

// utcWeekKeys builds the continuous list of Monday week_start keys across
// [start, end] in UTC, matching the backend's date_trunc('week', … AT TIME ZONE
// 'UTC'). Computing (and keying) in UTC — not the browser's local zone — is what
// keeps the gap-fill aligned with the backend buckets; a local-zone Monday can
// format to the adjacent day and never match, dropping that week (MEKO-605).
function utcWeekKeys(start: Date, end: Date): string[] {
  const day = new Date(Date.UTC(start.getUTCFullYear(), start.getUTCMonth(), start.getUTCDate()));
  day.setUTCDate(day.getUTCDate() - ((day.getUTCDay() + 6) % 7)); // back to Monday
  const keys: string[] = [];
  for (; day.getTime() <= end.getTime(); day.setUTCDate(day.getUTCDate() + 7)) {
    keys.push(day.toISOString().slice(0, 10));
  }
  return keys;
}

export function DatapacksByWeekChart({ data, start, end, isLoading }: Props) {
  // The backend returns only weeks that had datapacks; build a continuous
  // Monday-week axis across the range so gaps render as zero-height bars.
  const counts = new Map((data ?? []).map((d) => [d.week_start, d.count]));
  const weeks =
    start && end
      ? utcWeekKeys(start, end).map((key) => ({ week: weekLabel(key), count: counts.get(key) ?? 0 }))
      : (data ?? []).map((d) => ({ week: weekLabel(d.week_start), count: d.count }));

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium">Datapacks created per week</CardTitle>
        <Layers className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-56 w-full" />
        ) : weeks.length === 0 ? (
          <div className="flex h-56 items-center justify-center text-sm text-muted-foreground">
            No datapacks created in this range
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={240}>
            <BarChart data={weeks} margin={{ top: 4, right: 12, left: -8, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" className="stroke-border" vertical={false} />
              <XAxis
                dataKey="week"
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
                interval="preserveStartEnd"
                minTickGap={16}
              />
              <YAxis
                allowDecimals={false}
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
                width={32}
              />
              <Tooltip
                contentStyle={{
                  backgroundColor: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "6px",
                  fontSize: "12px",
                }}
                cursor={{ fill: "hsl(var(--accent))" }}
              />
              <Bar dataKey="count" name="Datapacks" fill={CHART_ORANGE} radius={[3, 3, 0, 0]} />
            </BarChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  );
}
