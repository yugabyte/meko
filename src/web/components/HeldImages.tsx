import type { HeldImages as Held } from '../heldImages';
import { cx, keepFocus } from '../util';

/**
 * Images waiting to go to Claude, each with a ✕ to take it out: a terminal's (sent with the next
 * Enter, or the compose box's Send on touch screens; under the terminal on a desktop, above the
 * compose box on touch screens) or the New session dialog's (sent with the first prompt).
 */
export function HeldImages({ held, hint }: { held: Held<Blob>; hint: string }) {
  const images = held.list;
  if (!images.length) return null;
  const count = images.length === 1 ? '1 image' : `${images.length} images`;
  return (
    <div className="held-images" role="group" aria-label="Images to send">
      <div className="held-list">
        {images.map((img, i) => (
          <div key={img.id} className={cx('held', !img.path && 'uploading')}>
            <img src={img.thumb} alt={img.path ? `Image ${i + 1}` : `Image ${i + 1}, uploading`} />
            <button
              type="button"
              className="held-remove"
              aria-label={`Remove image ${i + 1}`}
              title="Remove"
              onPointerDown={keepFocus}
              onMouseDown={keepFocus}
              onClick={() => held.remove(img.id)}
            >
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" aria-hidden="true">
                <path d="M6 6l12 12 M18 6L6 18" />
              </svg>
            </button>
          </div>
        ))}
      </div>
      <span className="held-hint" aria-live="polite">
        {count} {hint}
      </span>
    </div>
  );
}
