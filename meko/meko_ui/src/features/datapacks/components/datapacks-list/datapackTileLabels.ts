// Label helpers shared by the datapack tile and the list around it. Pure
// string derivation, kept out of the components so both can use them without
// one importing the other.

import type { CurrentUser, Datapack } from "@/lib/api/mekoApi";
import {
  formatDatapackShareGrantCamelCase,
  isDatapackShareGrant,
} from "@/lib/datapackShareGrants";

/** Tile variants. `shared` tints the card and adds the "Shared by" strip. */
export const OWNED = "owned";
export const SHARED = "shared";

export type TileVariant = "owned" | "shared";

/** Short subtitle under the datapack name (ellipsis when longer). */
const DESCRIPTION_PREVIEW_LEN = 72;

export function datapackDescriptionPreview(dp: Datapack): { preview: string; title?: string } {
  const raw = (dp.description ?? "").trim();
  if (!raw) {
    return { preview: "No description" };
  }
  if (raw.length <= DESCRIPTION_PREVIEW_LEN) {
    return { preview: raw };
  }
  return {
    preview: `${raw.slice(0, DESCRIPTION_PREVIEW_LEN)}...`,
    title: raw,
  };
}

export function datapackAccessFooterLabel(dp: Datapack): string {
  const g = dp.grant;
  if (g != null && isDatapackShareGrant(g)) return formatDatapackShareGrantCamelCase(g);
  return "—";
}

/** Who shared it: email when known, else the display name. */
export function datapackSharedByLabel(dp: Datapack): string {
  const email = (dp.datapack_owner_email_id ?? "").trim();
  if (email) return email;
  const name = (dp.datapack_owner_name ?? "").trim();
  if (name) return name;
  return "—";
}

/** Owner line for tiles: "Me" when the signed-in account owns the row. */
export function datapackTileOwnerLabel(dp: Datapack, currentUser: CurrentUser | null): string {
  if (
    currentUser &&
    dp.user_id != null &&
    dp.user_id !== "" &&
    dp.user_id === currentUser.account_id
  ) {
    return "Me";
  }
  const name = (dp.datapack_owner_name ?? "").trim();
  if (name) return name;
  return "—";
}
