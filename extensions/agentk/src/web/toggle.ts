// On/off settings kept in this browser (Settings › General and Harness): stored only while they differ from
// their default, and applied as they're ticked, here and in the browser's other tabs.
import { useEffect, useState } from 'react';

export interface BrowserToggle {
  /** The setting now: this page's last save, else what's stored, else the default. */
  saved(): boolean;
  save(on: boolean): void;
  /** The setting, kept current as it changes here (Settings) or in another tab. */
  use(): boolean;
}

export function browserToggle(key: string, byDefault: boolean): BrowserToggle {
  const changed = `${key}-changed`;
  // Set by a save in this page, so the setting holds here when storage can't keep it; another
  // tab's change (a storage event) clears it.
  let here: boolean | null = null;

  const saved = (): boolean => {
    if (here !== null) return here;
    try {
      const v = localStorage.getItem(key);
      return v === null ? byDefault : v === '1';
    } catch {
      return byDefault;
    }
  };

  const save = (on: boolean): void => {
    here = on;
    try {
      if (on === byDefault) localStorage.removeItem(key);
      else localStorage.setItem(key, on ? '1' : '0');
    } catch {
      /* storage unavailable: it holds for this page */
    }
    window.dispatchEvent(new Event(changed));
  };

  const use = (): boolean => {
    const [on, setOn] = useState(saved);
    useEffect(() => {
      const update = () => setOn(saved());
      const onStorage = (e: StorageEvent) => {
        if (e.key !== key && e.key !== null) return;
        here = null;
        update();
      };
      window.addEventListener(changed, update);
      window.addEventListener('storage', onStorage);
      return () => {
        window.removeEventListener(changed, update);
        window.removeEventListener('storage', onStorage);
      };
    }, []);
    return on;
  };

  return { saved, save, use };
}

/** Whether session tabs and rail rows show their agent's logo while open sessions use two agents. On by default. */
export const harnessLogosToggle = browserToggle('ak-harness-logos', true);
