// Shown when the workspace has no collections yet. Built on the shared Empty
// primitive so it matches every other zero state in the app.

import { FolderPlus } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty";
import {
  NEW_COLLECTION_LABEL,
  COLLECTIONS_EMPTY_BODY,
  COLLECTIONS_EMPTY_TITLE,
} from "@/features/collections/collectionsCopy";

export function CollectionsEmptyState({ onCreate }: { onCreate: () => void }) {
  return (
    <Empty className="rounded-xl border border-dashed border-border bg-card/40">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <FolderPlus className="h-6 w-6" aria-hidden />
        </EmptyMedia>
        <EmptyTitle>{COLLECTIONS_EMPTY_TITLE}</EmptyTitle>
        <EmptyDescription>{COLLECTIONS_EMPTY_BODY}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button type="button" onClick={onCreate} className="gap-2">
          <FolderPlus className="h-4 w-4" />
          {NEW_COLLECTION_LABEL}
        </Button>
      </EmptyContent>
    </Empty>
  );
}
