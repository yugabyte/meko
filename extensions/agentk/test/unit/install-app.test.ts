import { describe, expect, it } from 'vitest';
import { installOffer, isIos } from '../../src/web/installapp.js';

describe('offering to install AgentK as an app', () => {
  const none = { installed: false, canPrompt: false, ios: false, dismissed: false };

  it.each([
    ['the browser can install it', { canPrompt: true }, 'prompt'],
    ['an iPhone or iPad, which has no offer', { ios: true }, 'ios'],
    ['the browser offers it on iOS too', { canPrompt: true, ios: true }, 'prompt'],
    ['already installed', { canPrompt: true, installed: true }, null],
    ['the installed app on iOS', { ios: true, installed: true }, null],
    ['turned down in this browser', { canPrompt: true, dismissed: true }, null],
    ['turned down on iOS', { ios: true, dismissed: true }, null],
    ['no offer and no instructions (Firefox, desktop Safari)', {}, null],
  ])('%s', (_name, env, offer) => {
    expect(installOffer({ ...none, ...env })).toBe(offer);
  });

  it.each([
    ['iPhone', 'iPhone', 'Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X)', 5, true],
    ['iPad on iPadOS, which says it is a Mac', 'MacIntel', 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)', 5, true],
    ['a Mac', 'MacIntel', 'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)', 0, false],
    ['Android', 'Linux armv81', 'Mozilla/5.0 (Linux; Android 15; Pixel 9)', 5, false],
    ['Chrome on Windows', 'Win32', 'Mozilla/5.0 (Windows NT 10.0; Win64; x64)', 0, false],
  ])('%s is iOS: %s', (_name, platform, ua, touch, ios) => {
    expect(isIos(platform, ua, touch)).toBe(ios);
  });
});
