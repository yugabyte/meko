import { useMemo, useState } from 'react';
import { parseWorkLink } from '../../shared/worklinks';
import { WORK_NOTES_MAX, WORK_TITLE_MAX, type WorkItem } from '../../shared/types';
import { api, errorMessage } from '../api';
import { useTouch } from '../hooks/useMedia';
import { Busy, Dialog, ErrorText, Field } from './common';
import { WorkLinkChip } from './WorkQueueView';

/** Adds a work item, or edits one: its title, its links (one a line, shown as chips as they're read) and notes. */
export function WorkItemDialog({ item, onClose }: { item?: WorkItem; onClose: () => void }) {
  const touch = useTouch();
  const [title, setTitle] = useState(item?.title ?? '');
  const [links, setLinks] = useState(item?.links.map((l) => l.url ?? l.label).join('\n') ?? '');
  const [notes, setNotes] = useState(item?.notes ?? '');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const typed = useMemo(() => links.split('\n').map((l) => l.trim()).filter(Boolean), [links]);
  const read = useMemo(() => typed.map((t) => ({ t, link: parseWorkLink(t) })), [typed]);
  const bad = read.find((r) => !r.link);
  const ghFirst = read[0]?.link?.kind === 'github';
  const needsTitle = !title.trim() && (!!item || !ghFirst);
  const blocker = bad ? `"${bad.t.slice(0, 60)}" isn't a link AgentK can read.` : needsTitle ? 'Give the work item a title.' : null;

  const save = async () => {
    if (blocker || busy) return;
    setError(null);
    setBusy(true);
    try {
      const b = { title: title.trim(), notes: notes.trim(), links: typed };
      if (item) await api.updateWorkItem(item.id, b);
      else await api.addWorkItem(b);
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={item ? 'Edit work item' : 'Add a work item'}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          <button className="btn primary" disabled={!!blocker || busy} title={blocker ?? undefined} onClick={() => void save()}>
            {busy ? <Busy label={item ? 'Saving' : 'Adding'} /> : item ? 'Save' : 'Add'}
          </button>
        </>
      }
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          void save();
        }}
      >
        <Field
          label="Title"
          hint={needsTitle ? (item ? 'Give the work item a title.' : 'Give it a title, or put a GitHub PR or issue link first to use its title.') : undefined}
        >
          <input
            id="work-item-title"
            className="input"
            value={title}
            maxLength={WORK_TITLE_MAX}
            autoComplete="off"
            autoFocus={!touch}
            onChange={(e) => setTitle(e.target.value)}
          />
        </Field>
        <Field label="Links" hint="One a line: GitHub PRs and issues, Jira issues, Slack messages, or any other link.">
          <textarea id="work-item-links" className="input" rows={3} value={links} onChange={(e) => setLinks(e.target.value)} />
        </Field>
        {read.some((r) => r.link) && (
          <div className="wq-links">
            {read.map((r) => r.link && <WorkLinkChip key={r.link.key} link={r.link} />)}
          </div>
        )}
        <Field label="Notes">
          <textarea id="work-item-notes" className="input" rows={3} value={notes} maxLength={WORK_NOTES_MAX} onChange={(e) => setNotes(e.target.value)} />
        </Field>
        <ErrorText error={error ?? (bad ? blocker : null)} />
      </form>
    </Dialog>
  );
}
