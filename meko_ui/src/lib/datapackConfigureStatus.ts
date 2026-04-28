/**
 * Tracks whether the user pressed Finish on the configure wizard’s last step, per datapack.
 * Stored in localStorage until a server flag exists; keys use the route/datapack name (trimmed),
 * namespaced with encodeURIComponent.
 */
import { dispatchAppEvent } from "@/lib/browserEvents";

const STORAGE_PREFIX = "meko_datapack_configure_done:";

/** Fired after {@link markDatapackWizardConfigureDone} persists (same-tab listeners; storage event is cross-tab only). */
export const DATAPACK_WIZARD_CONFIGURE_DONE_EVENT = "meko:datapack-wizard-configure-done";

/** Full localStorage key for this datapack’s wizard-finished flag (trimmed name). */
export function configureWizardDoneStorageKey(datapackName: string): string {
  return STORAGE_PREFIX + encodeURIComponent(datapackName.trim());
}

export function isDatapackWizardConfigureDone(datapackName: string): boolean {
  const key = datapackName.trim();
  if (!key) return false;
  try {
    return window.localStorage.getItem(configureWizardDoneStorageKey(key)) === "1";
  } catch {
    return false;
  }
}

export function markDatapackWizardConfigureDone(datapackName: string): void {
  const key = datapackName.trim();
  if (!key) return;
  try {
    window.localStorage.setItem(configureWizardDoneStorageKey(key), "1");
    dispatchAppEvent(DATAPACK_WIZARD_CONFIGURE_DONE_EVENT, { datapackName: key });
  } catch {
    // Private mode / quota — wizard still works; detail page may keep showing Configure.
  }
}
