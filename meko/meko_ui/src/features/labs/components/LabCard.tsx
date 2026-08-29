import { Link } from "react-router-dom";
import { Clock } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

import { labDetailPath } from "@/app/routes/paths";

import type { LabDefinition } from "../types";

export function LabCard({ lab }: { lab: LabDefinition }) {
  const Icon = lab.icon;
  return (
    <Link to={labDetailPath(lab.id)}>
      <Card className="h-full transition-colors hover:border-primary/50">
        <CardHeader className="flex-row items-start gap-3 space-y-0">
          <div className="rounded-md bg-primary/10 p-2">
            <Icon className="h-5 w-5 text-primary" />
          </div>
          <div className="flex flex-col gap-1">
            <CardTitle className="text-base">{lab.title}</CardTitle>
            <div className="flex flex-wrap gap-1.5">
              {lab.tags.map((tag) => (
                <Badge key={tag} variant="secondary">
                  {tag}
                </Badge>
              ))}
            </div>
          </div>
        </CardHeader>
        <CardContent className="flex flex-col gap-2">
          <p className="text-sm text-muted-foreground">{lab.summary}</p>
          <p className="flex items-center gap-1 text-xs text-muted-foreground">
            <Clock className="h-3 w-3" /> ~{lab.estimatedMinutes} min
          </p>
        </CardContent>
      </Card>
    </Link>
  );
}
