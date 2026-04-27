import type { LucideIcon } from "lucide-react";
import { Crown, Eye, Pencil } from "lucide-react";

export type ShareInviteRoleId = "view" | "edit_memories" | "admin";

export const SHARE_INVITE_ROLES: {
  id: ShareInviteRoleId;
  label: string;
  description: string;
  icon: LucideIcon;
}[] = [
  { id: "view", label: "Can view", description: "Read knowledge, see memories", icon: Eye },
  {
    id: "edit_memories",
    label: "Can edit memories",
    description: "Add & edit own memories, propose Ko",
    icon: Pencil,
  },
  { id: "admin", label: "Admin", description: "Manage sharing, promote to Ko, delete", icon: Crown },
];
