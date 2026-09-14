// A lab that doesn't exist yet: same shape as LabCard, not clickable, and
// visibly dimmed so it reads as a roadmap entry rather than a broken link.

import { FileText } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

import type { UpcomingLab } from "../data/adlcStages";

export function UpcomingLabCard({ lab }: { lab: UpcomingLab }) {
  return (
    <Card className="h-full opacity-60">
      <CardHeader className="flex-row items-start gap-3 space-y-0">
        <div className="rounded-md bg-muted p-2">
          <FileText className="h-5 w-5 text-muted-foreground" aria-hidden />
        </div>
        <div className="flex flex-col gap-1">
          <CardTitle className="text-base text-muted-foreground">{lab.title}</CardTitle>
          <div className="flex flex-wrap gap-1.5">
            {lab.tags.map((tag) => (
              <Badge key={tag} variant="secondary">
                {tag}
              </Badge>
            ))}
            <Badge variant="secondary">Coming soon</Badge>
          </div>
        </div>
      </CardHeader>
      <CardContent>
        <p className="text-sm text-muted-foreground">{lab.summary}</p>
      </CardContent>
    </Card>
  );
}
