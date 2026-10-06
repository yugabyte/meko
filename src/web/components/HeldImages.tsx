import type { SessionTerminal } from '../terminals';
import { cx, keepFocus } from '../util';

/**
 * The images waiting to go to Claude with the next Enter (or the compose box's Send on touch
 * screens), each with a ✕ to take it out. Under the terminal on a desktop, above the compose box on
 * touch screens.
 */
export function HeldImages({ term, touch }: { term: SessionTerminal; touch: boolean }) {
  const images = term.held.list;
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
              onClick={() => term.held.remove(img.id)}
            >
              <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" aria-hidden="true">
                <path d="M6 6l12 12 M18 6L6 18" />
              </svg>
            </button>
          </div>
        ))}
      </div>
      <span className="held-hint" aria-live="polite">
        {count} {touch ? 'sent with your message' : 'sent with your next Enter'}
      </span>
    </div>
  );
}
