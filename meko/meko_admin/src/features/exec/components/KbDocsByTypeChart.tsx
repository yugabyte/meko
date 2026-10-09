// Indexed (RAG'ed) KB documents broken down by file type — one bar per document
// type present in the selected range. Mirrors ConversationsByAgentChart.
import {
  BarChart,
  Bar,
  LabelList,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
  ResponsiveContainer,
} from "recharts";
import { FileText } from "lucide-react";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Skeleton } from "@/components/ui/skeleton";
import { CHART_GREEN } from "@/features/dashboard/components/chartColors";
import type { ExecDocTypeCount } from "@/lib/adminApi";

type Props = {
  data: ExecDocTypeCount[] | undefined;
  isLoading: boolean;
};

// Stored MIME type -> short axis label. Unknown types fall back to the trailing
// MIME subtype so every distinct type still gets its own bar (no bucketing).
const MIME_LABELS: Record<string, string> = {
  "application/pdf": "PDF",
  "text/plain": "TXT",
  "text/markdown": "Markdown",
  "text/csv": "CSV",
  "text/html": "HTML",
  "application/json": "JSON",
  "application/vnd.openxmlformats-officedocument.wordprocessingml.document": "DOCX",
  unknown: "Unknown",
};

function typeLabel(mime: string): string {
  return MIME_LABELS[mime] ?? mime.split("/").pop() ?? mime;
}

export function KbDocsByTypeChart({ data, isLoading }: Props) {
  const rows = (data ?? []).map((d) => ({ type: typeLabel(d.type), count: d.count }));

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between pb-2">
        <CardTitle className="text-sm font-medium">Indexed documents by file type</CardTitle>
        <FileText className="h-4 w-4 text-muted-foreground" />
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <Skeleton className="h-48 w-full" />
        ) : rows.length === 0 ? (
          <div className="flex h-48 items-center justify-center text-sm text-muted-foreground">
            No indexed documents in this range
          </div>
        ) : (
          <ResponsiveContainer width="100%" height={Math.max(200, rows.length * 40)}>
            <BarChart
              data={rows}
              layout="vertical"
              margin={{ top: 4, right: 40, left: 0, bottom: 0 }}
            >
              <CartesianGrid strokeDasharray="3 3" className="stroke-border" horizontal={false} />
              <XAxis
                type="number"
                allowDecimals={false}
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
              />
              <YAxis
                type="category"
                dataKey="type"
                width={88}
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
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
              <Bar dataKey="count" name="Documents" fill={CHART_GREEN} radius={[0, 3, 3, 0]}>
                <LabelList
                  dataKey="count"
                  position="right"
                  className="fill-foreground"
                  fontSize={11}
                />
              </Bar>
            </BarChart>
          </ResponsiveContainer>
        )}
      </CardContent>
    </Card>
  );
}
