// Confirmation before resetting a datapack's share link: the current URL stops
// working at once, and people who already joined keep their access.
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

type Props = {
  open: boolean;
  resetting: boolean;
  onCancel: () => void;
  onConfirm: () => void;
};

export function ResetShareLinkDialog({ open, resetting, onCancel, onConfirm }: Props) {
  return (
    <AlertDialog open={open} onOpenChange={(o) => !o && !resetting && onCancel()}>
      <AlertDialogContent className="bg-card border-border">
        <AlertDialogHeader>
          <AlertDialogTitle className="text-foreground">Reset the share link?</AlertDialogTitle>
          <AlertDialogDescription className="text-muted-foreground">
            The current link stops working right away, and anyone you sent it to will need the new
            one. People who already joined keep their access; remove them from the Members page if
            they shouldn't.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={resetting}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={resetting}
            className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            onClick={(e) => {
              e.preventDefault();
              onConfirm();
            }}
          >
            Reset link
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
