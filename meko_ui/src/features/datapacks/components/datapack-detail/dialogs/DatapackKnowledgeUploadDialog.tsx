import {
  Close as DialogClose,
  Content as DialogContent,
  Description as DialogDescription,
  Overlay as DialogOverlay,
  Portal as DialogPortal,
  Root as DialogRoot,
  Title as DialogTitle,
} from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";
import FileUpload from "@/features/datapacks/components/FileUpload";

export type DatapackKnowledgeUploadDialogProps = {
  datapackName: string;
  datapackId: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Optional pre-staged files (e.g. a sample). Re-captured each time the dialog opens. */
  initialFiles?: File[];
  /** Fired after `createKnowledgeBase` succeeds inside the embedded
   *  FileUpload so the parent page can refetch derived data (summary
   *  counts, KB document list, …). Optional — pages that don't need
   *  to refresh anything can omit it. */
  onKnowledgeBaseCreated?: () => void;
};

export function DatapackKnowledgeUploadDialog({
  datapackName,
  datapackId,
  open,
  onOpenChange,
  initialFiles,
  onKnowledgeBaseCreated,
}: DatapackKnowledgeUploadDialogProps) {
  // Plain, default Radix Dialog -- no forceMount. Radix always unmounts
  // Content on close regardless, so state survival is handled inside
  // FileUpload itself (a cache outside the React tree), not here.
  return (
    <DialogRoot open={open} onOpenChange={onOpenChange}>
      <DialogPortal>
        <DialogOverlay className="fixed inset-0 z-50 bg-black/80 data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0" />
        <DialogContent
          className={cn(
            "fixed left-[50%] top-[50%] z-50 grid w-[calc(100vw-2rem)] max-w-2xl max-h-[calc(100vh-2rem)] translate-x-[-50%] translate-y-[-50%] gap-4 overflow-y-auto border border-border bg-card p-0 text-foreground shadow-lg duration-200 sm:rounded-lg",
            "data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-95 data-[state=open]:zoom-in-95 data-[state=closed]:slide-out-to-left-1/2 data-[state=closed]:slide-out-to-top-[48%] data-[state=open]:slide-in-from-left-1/2 data-[state=open]:slide-in-from-top-[48%]",
          )}
        >
          <div className="px-6 pt-6 pb-2 shrink-0 border-b border-border">
            <DialogTitle className="text-xl font-semibold leading-none tracking-tight text-foreground">
              Upload to knowledge base
            </DialogTitle>
            <DialogDescription className="text-sm text-muted-foreground mt-1.5">
              Add documents for <span className="text-foreground font-medium">{datapackName}</span>.
              Supported types and limits follow the uploader below.
            </DialogDescription>
          </div>
          <div className="px-4 py-4 sm:px-6 sm:pb-6">
            {/* key: remounts on datapack switch so state never leaks. */}
            <FileUpload
              key={datapackId}
              datapackName={datapackName}
              datapackId={datapackId}
              initialFiles={initialFiles}
              onKnowledgeBaseCreated={onKnowledgeBaseCreated}
            />
          </div>
          <DialogClose className="absolute right-4 top-4 rounded-sm opacity-70 ring-offset-background transition-opacity hover:opacity-100 focus:outline-none focus:ring-2 focus:ring-ring focus:ring-offset-2 disabled:pointer-events-none data-[state=open]:bg-accent data-[state=open]:text-muted-foreground">
            <X className="h-4 w-4" />
            <span className="sr-only">Close</span>
          </DialogClose>
        </DialogContent>
      </DialogPortal>
    </DialogRoot>
  );
}
