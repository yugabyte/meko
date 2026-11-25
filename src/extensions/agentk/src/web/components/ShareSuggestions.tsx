import type { ShareContact } from '../../shared/types';
import { pickEmail, suggestContacts, typedEntry } from '../../shared/emails';

/** Most people suggested at once. */
const MAX_SUGGESTIONS = 6;

/**
 * The Share dialog's suggestions under the Emails field: the people invited before (every
 * project's, most often and lately first), or, while an entry is being typed, those it matches.
 * Each adds its address to the field; × stops suggesting them. Hidden when there's no one to offer.
 */
export function ShareSuggestions({
  value,
  contacts,
  exclude,
  onPick,
  onForget,
}: {
  /** The Emails field. */
  value: string;
  contacts: ShareContact[];
  /** Lower-case addresses not to offer: with access, invited, or already in the field. */
  exclude: Set<string>;
  /** The field with the address picked. */
  onPick: (value: string) => void;
  onForget: (email: string) => void;
}) {
  const { query } = typedEntry(value);
  const list = suggestContacts(contacts, { exclude, query, limit: MAX_SUGGESTIONS });
  if (!list.length) return null;
  return (
    <div className="share-suggest" role="group" aria-label="People you've shared with">
      <span className="share-suggest-head">{query ? 'Matches' : 'Frequent'}</span>
      {list.map((c) => (
        <span className="share-chip" key={c.email}>
          <button
            type="button"
            className="share-chip-add"
            aria-label={`Add ${c.name ? `${c.name}, ` : ''}${c.email}`}
            onClick={() => onPick(pickEmail(value, c.email))}
          >
            {c.name ?? c.email}
            {c.name && <span className="share-chip-email"> {c.email}</span>}
          </button>
          <button type="button" className="share-chip-forget" aria-label={`Stop suggesting ${c.email}`} onClick={() => onForget(c.email)}>
            ×
          </button>
        </span>
      ))}
    </div>
  );
}
