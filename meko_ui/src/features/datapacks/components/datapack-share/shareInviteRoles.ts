// Central definitions for datapack share roles shown in invites, member selects, and pending rows.
// IDs must stay aligned with the API DatapackShareGrant union in mekoApi; helpers map grants to UI copy.
import type { LucideIcon } from "lucide-react";
import { Crown, Eye, Pencil } from "lucide-react";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";
import {
  DATAPACK_SHARE_GRANT_CONTRIBUTOR,
  DATAPACK_SHARE_GRANT_MAINTAINER,
  DATAPACK_SHARE_GRANT_VIEWER,
} from "@/lib/datapackShareGrants";

/** UI / API role id for invites (matches {@link DatapackShareGrant}). */
export type ShareInviteRoleId = DatapackShareGrant;

export const SHARE_INVITE_ROLES: {
  id: ShareInviteRoleId;
  label: string;
  description: string;
  icon: LucideIcon;
}[] = [
    {
      id: DATAPACK_SHARE_GRANT_VIEWER,
      label: "Viewer",
      description: "Can view Knowledge, memories, conversations",
      icon: Eye,
    },
    {
      id: DATAPACK_SHARE_GRANT_CONTRIBUTOR,
      label: "Contributor",
      description: "Includes viewer rights and can generate memories",
      icon: Pencil,
    },
    {
      id: DATAPACK_SHARE_GRANT_MAINTAINER,
      label: "Maintainer",
      description:
        "Includes contributor rights and can generate collective memories and knowledge",
      icon: Crown,
    },
  ];

/** Short label for chips and compact UI; falls back to raw grant if the API sends an unknown value. */
export function shareGrantLabel(grant: DatapackShareGrant): string {
  return SHARE_INVITE_ROLES.find((r) => r.id === grant)?.label ?? grant;
}

/** Longer copy for pending-invite rows and role tiles (empty string if grant is unknown). */
export function shareGrantDescription(grant: DatapackShareGrant): string {
  return SHARE_INVITE_ROLES.find((r) => r.id === grant)?.description ?? "";
}
