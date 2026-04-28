import { useCallback, useSyncExternalStore } from "react";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import {
  configureWizardDoneStorageKey,
  DATAPACK_WIZARD_CONFIGURE_DONE_EVENT,
  isDatapackWizardConfigureDone,
} from "@/lib/datapackConfigureStatus";

/**
 * Live view of “configure wizard finished” for this datapack (localStorage + same-tab event).
 *
 * - {@link DATAPACK_WIZARD_CONFIGURE_DONE_EVENT}: same tab when Finish marks done.
 * - `storage`: other tabs / windows changing localStorage (same-tab changes do **not** fire `storage`).
 * - `window` `focus`: recover after same-tab edits (e.g. clearing a key in DevTools) — not a culprit for
 *   hiding Configure; it only triggers a fresh read so the menu can show again if the key was removed.
 */
function subscribe(datapackName: string, onStoreChange: () => void) {
  const trimmed = datapackName.trim();
  if (!trimmed) return () => undefined;

  const expectedKey = configureWizardDoneStorageKey(datapackName);

  const onCustom = (event: Event) => {
    const name = (event as CustomEvent<{ datapackName?: string }>).detail?.datapackName?.trim();
    if (name === trimmed) onStoreChange();
  };

  const onStorage = (e: StorageEvent) => {
    if (e.key === expectedKey) onStoreChange();
  };

  const onFocus = () => onStoreChange();

  addAppEventListener(DATAPACK_WIZARD_CONFIGURE_DONE_EVENT, onCustom);
  window.addEventListener("storage", onStorage);
  window.addEventListener("focus", onFocus);

  return () => {
    removeAppEventListener(DATAPACK_WIZARD_CONFIGURE_DONE_EVENT, onCustom);
    window.removeEventListener("storage", onStorage);
    window.removeEventListener("focus", onFocus);
  };
}

export function useDatapackWizardConfigureDone(datapackName: string): boolean {
  return useSyncExternalStore(
    useCallback((onStoreChange) => subscribe(datapackName, onStoreChange), [datapackName]),
    useCallback(() => isDatapackWizardConfigureDone(datapackName), [datapackName]),
    () => false,
  );
}
