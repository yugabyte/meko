import { useCallback, useEffect, useRef, useState } from 'react';
import type { LinkGrant, ShareLinkChange, ShareLinkState } from '../../shared/types';
import { LINK_GRANTS } from '../../shared/types';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';
import { cx } from '../util';
import { Busy, ConfirmButton, ErrorText } from './common';

const GRANT_TITLE: Record<LinkGrant, string> = { viewer: 'Viewer', contributor: 'Contributor' };

/**
 * The Share dialog's Share by link section: the datapack's one Meko share link. Off, you pick the
 * role it gives before turning it on; on, its URL to copy, its role, Turn off and Reset link.
 * Its busy and error states are its own, so the invite form beside it keeps working.
 */
export function ShareLink({ datapackId }: { datapackId: string }) {
  const [state, setState] = useState<ShareLinkState | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  /** The role picked for turning the link on: none until the user picks one. */
  const [pick, setPick] = useState<LinkGrant | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const urlInput = useRef<HTMLInputElement>(null);

  const load = useCallback(() => {
    setLoadError(null);
    return api
      .datapackShareLink(datapackId)
      .then(setState)
      .catch((e) => setLoadError(errorMessage(e)));
  }, [datapackId]);

  useEffect(() => {
    void load();
  }, [load]);

  const change = async (key: string, body: ShareLinkChange) => {
    setBusy(key);
    setError(null);
    setNote(null);
    try {
      setState({ link: await api.setDatapackShareLink(datapackId, body) });
      setPick(null);
      if (key === 'reset') setNote('Reset. The old link no longer works.');
    } catch (e) {
      setError(errorMessage(e));
      // Part of a change may have gone through (turned on, but not given the role): show the link as it is.
      void api.datapackShareLink(datapackId).then(setState, () => undefined);
    } finally {
      setBusy(null);
    }
  };

  const copy = async (url: string) => {
    setError(null);
    if (await copyText(url)) {
      setNote('Copied.');
      return;
    }
    urlInput.current?.select();
    setNote(null);
    setError("The browser didn't let AgentK copy it. It's selected in the field: copy it from there.");
  };

  if (loadError) {
    return (
      <div className="field share-link">
        <span className="label">Share by link</span>
        <p className="error-text">{loadError}</p>
        <span>
          <button type="button" className="btn small" onClick={() => void load()}>
            Try again
          </button>
        </span>
      </div>
    );
  }
  if (!state) {
    return (
      <div className="field share-link">
        <span className="label">Share by link</span>
        <p className="muted">
          <Busy label="Loading" />
        </p>
      </div>
    );
  }
  if (!state.link) {
    // An older or self-hosted Meko without share links: nothing to offer.
    if (state.unavailable === 'unsupported') return null;
    return (
      <div className="field share-link">
        <span className="label">Share by link</span>
        <p className="hint">
          {state.unavailable === 'permission'
            ? 'Only the datapack’s owner or a maintainer can share it by link.'
            : 'Meko’s web app URL isn’t known, so AgentK can’t make a share link. Set it on the Meko tab in Settings.'}
        </p>
      </div>
    );
  }

  const link = state.link;
  const shown = link.active ? link.grant : pick;
  const roles = (
    <div className="share-link-row">
      <span className="hint" id="share-link-role">
        They join as
      </span>
      <div className="seg" role="radiogroup" aria-labelledby="share-link-role">
        {LINK_GRANTS.map((g) => (
          <button
            type="button"
            key={g}
            role="radio"
            aria-checked={shown === g}
            className={cx(shown === g && 'on')}
            disabled={busy !== null}
            onClick={() => (link.active ? g !== link.grant && void change('grant', { grant: g }) : setPick(g))}
          >
            {busy === 'grant' && shown !== g ? <Busy label={GRANT_TITLE[g]} /> : GRANT_TITLE[g]}
          </button>
        ))}
      </div>
    </div>
  );

  return (
    <div className="field share-link">
      <div className="share-link-row share-link-head">
        <span className="label">Share by link</span>
        {link.joined > 0 && <span className="hint">{link.joined === 1 ? '1 joined through it' : `${link.joined} joined through it`}</span>}
      </div>
      {!link.active || !link.url ? (
        <>
          <span className="hint">Anyone with the link can join after signing in to Meko. No invite needed.</span>
          {roles}
          <div className="share-link-row">
            <button
              type="button"
              className="btn primary"
              disabled={!pick || busy !== null}
              onClick={() => pick && void change('on', { active: true, grant: pick })}
            >
              {busy === 'on' ? <Busy label="Turning on" /> : 'Turn on link'}
            </button>
            {!pick && <span className="hint">Pick a role first.</span>}
          </div>
        </>
      ) : (
        <>
          <div className="share-link-row">
            <input
              ref={urlInput}
              id="share-link-url"
              className="input mono share-link-url"
              readOnly
              aria-label="Share link"
              value={link.url}
              onFocus={(e) => e.currentTarget.select()}
            />
            <button type="button" className="btn primary" onClick={() => void copy(link.url!)}>
              Copy link
            </button>
          </div>
          {roles}
          {link.atLinkRole > 0 && (
            <span className="hint">
              Changing the role also changes it for the {link.atLinkRole === 1 ? 'person' : `${link.atLinkRole} people`} who joined through this link.
            </span>
          )}
          <div className="share-link-row">
            <button type="button" className="btn small" disabled={busy !== null} onClick={() => void change('off', { active: false })}>
              {busy === 'off' ? <Busy label="Turning off" /> : 'Turn off'}
            </button>
            <ConfirmButton
              className="btn small"
              label="Reset link"
              confirmLabel="Reset link"
              prompt="The old link stops working. People who joined keep access; remove them in Meko."
              danger
              disabled={busy !== null}
              onConfirm={() => change('reset', { reset: true })}
            />
          </div>
        </>
      )}
      {note && <span className="ok-text">{note}</span>}
      <ErrorText error={error} />
    </div>
  );
}
