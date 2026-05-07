import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import FileUpload from "@/features/datapacks/components/FileUpload";

export type DatapackKnowledgeUploadDialogProps = {
  datapackName: string;
  datapackId?: string;
  open: boolean;
  onOpenChange: (open: boolean) => void;
};

export function DatapackKnowledgeUploadDialog({
  datapackName,
  datapackId,
  open,
  onOpenChange,
}: DatapackKnowledgeUploadDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-2xl max-h-[calc(100vh-2rem)] overflow-y-auto bg-[#1C2333] border-border text-foreground p-0 gap-0">
        <DialogHeader className="px-6 pt-6 pb-2 shrink-0 border-b border-border">
          <DialogTitle className="text-xl text-white">Upload to knowledge base</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Add documents for <span className="text-foreground font-medium">{datapackName}</span>. Supported types and
            limits follow the uploader below.
          </DialogDescription>
        </DialogHeader>
        <div className="px-4 py-4 sm:px-6 sm:pb-6">
          <FileUpload datapackName={datapackName} datapackId={datapackId} />
        </div>
      </DialogContent>
    </Dialog>
  );
}
