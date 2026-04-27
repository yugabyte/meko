/**
 * Tracks whether the user finished the post-create configure wizard (Status step), per datapack.
 * Stored in localStorage until a server flag exists; keys are namespaced by encoded datapack name.
 */
const STORAGE_PREFIX = "meko_datapack_configure_done:";

export function isDatapackWizardConfigureDone(datapackName: string): boolean {
  const key = datapackName.trim();
  if (!key) return false;
  try {
    return window.localStorage.getItem(STORAGE_PREFIX + encodeURIComponent(key)) === "1";
  } catch {
    return false;
  }
}

export function markDatapackWizardConfigureDone(datapackName: string): void {
  const key = datapackName.trim();
  if (!key) return;
  try {
    window.localStorage.setItem(STORAGE_PREFIX + encodeURIComponent(key), "1");
  } catch {
    // Private mode / quota — wizard still works; detail page may keep showing Configure.
  }
}
