import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { GH_NEEDED, type GhStatus } from '../../src/shared/types.js';
import { api } from '../../src/web/api';
import { closeGhDialog, ghDialogReason } from '../../src/web/ghdialog';
import { GhDialog } from '../../src/web/components/GhDialog';

const answer = (status: number, body: object) => vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify(body), { status })));

describe('the GitHub dialog', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    closeGhDialog();
  });

  it("opens for any call refused because gh isn't usable, with the refusal's words", async () => {
    answer(409, { error: 'Open project failed.' });
    await expect(api.ghCheck()).rejects.toThrow('Open project failed.');
    expect(ghDialogReason()).toBeNull();
    const why = "Open PR needs gh, GitHub's command-line tool, which isn't installed: install it from Kode, or from https://cli.github.com.";
    answer(409, { error: why, code: GH_NEEDED });
    await expect(api.sessionAction('s1', 'open-pr')).rejects.toThrow(why);
    expect(ghDialogReason()).toBe(why);
  });

  it("shows the sign-in's one-time code and links to GitHub's page for it", () => {
    const gh: GhStatus = { state: 'signed-out', login: null, job: { kind: 'login', phase: 'code', code: 'F364-DFAB', url: 'https://github.com/login/device' } };
    const html = renderToStaticMarkup(createElement(GhDialog, { reason: 'Merge needs gh signed in to GitHub.', gh, onClose: () => undefined }));
    expect(html).toContain('F364-DFAB');
    expect(html).toContain('href="https://github.com/login/device"');
  });
});
