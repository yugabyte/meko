import type { ReactNode } from 'react';
import type { HeldImages as Held } from '../heldImages';
import { fileBadge } from '../knowledge';
import { cx, keepFocus } from '../util';

/** "1 image", "2 files", "1 image and 2 files". */
function countText(images: number, files: number): string {
  const n = (k: number, one: string) => `${k} ${one}${k === 1 ? '' : 's'}`;
  return [images && n(images, 'image'), files && n(files, 'file')].filter(Boolean).join(' and ');
}

/**
 * Images and files waiting to go to Claude, each with a ✕ to take it out: a terminal's (sent with
 * the next Enter, or the compose box's Send on touch screens; under the terminal on a desktop, above
 * the compose box on touch screens) or the New session dialog's (images only, sent with the first
 * prompt). An image shows as a thumbnail, any other file as a tile with its extension. `actions`
 * go after the hint (the terminal's Upload to Meko).
 */
export function HeldImages({ held, hint, actions }: { held: Held<Blob>; hint: string; actions?: ReactNode }) {
  const items = held.list;
  if (!items.length) return null;
  const images = items.filter((i) => i.thumb).length;
  return (
    <div className="held-images" role="group" aria-label="Images and files to send">
      <div className="held-list">
        {items.map((item, i) => {
          const label = item.thumb ? `Image ${i + 1}` : item.name || `File ${i + 1}`;
          return (
            <div key={item.id} className={cx('held', !item.thumb && 'file', !item.path && 'uploading')} title={item.name || undefined}>
              {item.thumb ? (
                <img src={item.thumb} alt={item.path ? label : `${label}, uploading`} />
              ) : (
                // The name shows on the tile: a phone has no hover for its title.
                <span className="held-file" role="img" aria-label={item.path ? label : `${label}, uploading`}>
                  <span className="held-file-ext">{fileBadge(item.name)}</span>
                  <span className="held-file-name">{item.name || label}</span>
                </span>
              )}
              <button
                type="button"
                className="held-remove"
                aria-label={`Remove ${label}`}
                title="Remove"
                onPointerDown={keepFocus}
                onMouseDown={keepFocus}
                onClick={() => held.remove(item.id)}
              >
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" aria-hidden="true">
                  <path d="M6 6l12 12 M18 6L6 18" />
                </svg>
              </button>
            </div>
          );
        })}
      </div>
      <span className="held-hint" aria-live="polite">
        {countText(images, items.length - images)} {hint}
      </span>
      {actions}
    </div>
  );
}
