import { resolveMeko } from './config.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';
import { tryMekoToken, type TokenTry } from './meko.js';
import { latestMekoSetup, manualMekoSetup, mekoClaudeGaps, readClaudeMeko, runMekoSetup, type MekoSetupClient } from './mekosetup.js';
import { claudeUserDir } from './claude.js';
import { kiroMekoGaps } from './mekokiro.js';
import { HttpError } from './projects.js';

/**
 * What Settings › Harness needs to set Meko's capture up in the agents with AgentK's Meko; tests swap
 * them out. Their `meko` MCP server is Meko's: the page connects it through Meko's own route first.
 */
export interface MekoTools {
  /**
   * Runs Meko's setup for Claude Code against this server and key; rejects when it doesn't finish,
   * naming `manual`, the command to run by hand (`manualMekoSetup`).
   */
  setUpClaude: (url: string, apiKey: string, log: (line: string) => void, manual: string) => Promise<void>;
  /** Runs Meko's setup for Kiro (`--client kiro`) against this server and key; rejects when it doesn't finish. */
  setUpKiro: (url: string, apiKey: string, log: (line: string) => void, manual: string) => Promise<void>;
  /** Tries Claude Code's token on a Meko server: the datapacks it sees, or why not. */
  tryToken: (url: string, token: string) => Promise<TokenTry>;
}

export const defaultMekoTools: MekoTools = {
  async setUpClaude(url, apiKey, log, manual) {
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, log });
    const left = mekoClaudeGaps(readClaudeMeko(claudeUserDir()), url);
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} Run it yourself: ${manual}`);
    }
  },
  async setUpKiro(url, apiKey, log, manual) {
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, client: 'kiro', log });
    const left = kiroMekoGaps();
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup for Kiro didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} Run it yourself: ${manual}`);
    }
  },
  tryToken: tryMekoToken,
};

/** The command to run Meko's setup by hand, with the Meko on this computer's `meko token`. */
function manualFor(core: Core, url: string, client: MekoSetupClient): string {
  return manualMekoSetup(url, client, { home: core.mekoHome() });
}

/** Why a setup can't run: there's no Meko to set up with. */
const NO_MEKO = "Meko isn't running on this computer: start it with meko start.";

/**
 * Runs Meko's setup for Claude Code with AgentK's Meko, only while what Meko's capture needs is
 * missing (`MekoStatus.claude.capture`, checked again first); false when it had nothing to do.
 * AgentK keeps no key of its own: Meko owns the link.
 */
export async function setUpClaudeMeko(core: Core, tools: MekoTools): Promise<boolean> {
  const meko = resolveMeko();
  if (!meko.apiKey) throw new HttpError(400, NO_MEKO);
  await core.claudeMeko.refresh(true);
  if (!core.claudeMeko.status.capture.length) return false;
  const tail: string[] = [];
  try {
    await tools.setUpClaude(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup: ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    }, manualFor(core, meko.url, 'claude-code'));
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
  core.log.info(`set up Meko for Claude Code (${meko.url})`);
  return true;
}

/**
 * Runs Meko's setup for Kiro with AgentK's Meko, only while the agent Kiro sessions are built from
 * doesn't load Kiro's MCP config (`kiroMekoGaps`); false when it had nothing to do. It makes its own
 * agent, with the capture hooks, Kiro's default (which Kiro sessions' `agentk` agent is built from),
 * and writes its MCP server and key in Kiro's MCP config too. AgentK's own config is left as it is:
 * Kiro keeps its key in its own file.
 */
export async function setUpKiroMeko(core: Core, tools: MekoTools): Promise<boolean> {
  const meko = resolveMeko();
  if (!meko.apiKey) throw new HttpError(400, NO_MEKO);
  if (!kiroMekoGaps().length) return false;
  const tail: string[] = [];
  try {
    await tools.setUpKiro(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup (kiro): ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    }, manualFor(core, meko.url, 'kiro'));
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
  core.log.info(`set up Meko for Kiro (${meko.url})`);
  return true;
}
