// One runnable lab on the Labs list: what it is, and what it costs you to
// run it (chapters and rough duration).

import { Link } from "react-router-dom";

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
        <CardContent className="flex flex-col gap-3">
          <p className="text-sm text-muted-foreground">{lab.summary}</p>
          <p className="flex flex-wrap items-center gap-1.5 text-xs text-muted-foreground">
            <span>
              {lab.chapters.length} chapter{lab.chapters.length === 1 ? "" : "s"}
            </span>
            <span aria-hidden>·</span>
            <span>~{lab.estimatedMinutes} min</span>
          </p>
        </CardContent>
      </Card>
    </Link>
  );
}
