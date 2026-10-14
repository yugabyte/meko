// Confirmation before an owner or maintainer takes a member's access away. Removal
// revokes access at once; only a new invite brings them back, not the share link.
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
import type { DatapackOutgoingMember } from "@/lib/api/mekoApi";
import { memberDisplayName } from "./memberDisplay";

type Props = {
  member: DatapackOutgoingMember | null;
  datapackName: string;
  removing: boolean;
  onCancel: () => void;
  onConfirm: (member: DatapackOutgoingMember) => void;
};

export function RemoveMemberDialog({ member, datapackName, removing, onCancel, onConfirm }: Props) {
  return (
    <AlertDialog open={member !== null} onOpenChange={(open) => !open && !removing && onCancel()}>
      <AlertDialogContent className="bg-card border-border">
        <AlertDialogHeader>
          <AlertDialogTitle className="text-foreground">
            Remove {member ? memberDisplayName(member) : ""}?
          </AlertDialogTitle>
          <AlertDialogDescription className="text-muted-foreground">
            They lose access to {datapackName} right away and can rejoin only through a new invite.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel disabled={removing}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            disabled={removing}
            className="bg-destructive text-destructive-foreground hover:bg-destructive/90"
            onClick={(e) => {
              e.preventDefault();
              if (member) onConfirm(member);
            }}
          >
            Remove
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
