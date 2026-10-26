import { resolveMeko } from './config.js';
import type { Core } from './core.js';
import { errMsg } from './log.js';
import { tryMekoToken, type TokenTry } from './meko.js';
import { claudeHasMekoMcpAsync, latestMekoSetup, manualMekoSetup, mekoClaudeGaps, readClaudeMeko, runMekoSetup } from './mekosetup.js';
import { claudeConfigPath } from './trust.js';
import { claudeUserDir } from './claude.js';
import { kiroAgentLoadsMcp, kiroMekoGaps, readKiroMeko } from './mekokiro.js';
import { HttpError } from './projects.js';

/** What Settings › Harness needs to set the agents up with AgentK's Meko; tests swap them out. */
export interface MekoTools {
  /**
   * Runs Meko's setup for Claude Code against this server and key; rejects when it doesn't finish,
   * with the command to run by hand (`mekoHome`: Meko's data folder, for its `meko token`).
   */
  setUpClaude: (url: string, apiKey: string, log: (line: string) => void, claudeCommand: string, mekoHome: string | null) => Promise<void>;
  /** Runs Meko's setup for Kiro (`--client kiro`) against this server and key; rejects when it doesn't finish. */
  setUpKiro: (url: string, apiKey: string, log: (line: string) => void, mekoHome: string | null) => Promise<void>;
  /** Tries one of Claude Code's tokens on a Meko server: the datapacks it sees, or why not. */
  tryToken: (url: string, token: string) => Promise<TokenTry>;
  /**
   * Claude Code (`[claude] command`) has a `meko` MCP server (`mcp get meko`), asked when
   * ~/.claude.json has no user-scope one; null when it couldn't be asked.
   */
  claudeHasMcp: (claudeCommand: string) => Promise<boolean | null>;
}

export const defaultMekoTools: MekoTools = {
  async setUpClaude(url, apiKey, log, claudeCommand, mekoHome) {
    const manual = manualMekoSetup(url, 'claude-code', mekoHome);
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, log });
    const c = readClaudeMeko(claudeUserDir(), claudeConfigPath());
    const registered = c.server ? true : await claudeHasMekoMcpAsync(claudeCommand);
    const left = mekoClaudeGaps(c, url, () => registered);
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} Run it yourself: ${manual}`);
    }
  },
  async setUpKiro(url, apiKey, log, mekoHome) {
    const manual = manualMekoSetup(url, 'kiro', mekoHome);
    const pkg = await latestMekoSetup().catch((e: unknown) => {
      throw new Error(`Couldn't look up Meko's setup on npm (${errMsg(e)}). Run it yourself: ${manual}`);
    });
    const code = await runMekoSetup(pkg, { url, apiKey, client: 'kiro', log });
    const left = kiroMekoGaps(readKiroMeko(), url, kiroAgentLoadsMcp());
    if (code !== 0 || left.length) {
      throw new Error(`Meko's setup for Kiro didn't finish.${left.length ? ` Still: ${left.join(' ')}` : ''} Run it yourself: ${manual}`);
    }
  },
  tryToken: tryMekoToken,
  claudeHasMcp: claudeHasMekoMcpAsync,
};

/** Why a setup can't run: there's no Meko to set up with. */
const NO_MEKO = "Meko isn't set up: start Meko on this computer (meko start), or link Meko Cloud in Meko's web UI (Settings › Meko Cloud).";

/** Runs Meko's setup for Claude Code with AgentK's Meko. AgentK keeps no key of its own: Meko owns the link. */
export async function setUpClaudeMeko(core: Core, tools: MekoTools): Promise<void> {
  const meko = resolveMeko(core.config);
  if (!meko.apiKey) throw new HttpError(400, NO_MEKO);
  const tail: string[] = [];
  try {
    await tools.setUpClaude(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup: ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    }, core.config.claude.command, core.mekoHome());
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
  core.log.info(`set up Meko for Claude Code (${meko.url})`);
}

/**
 * Runs Meko's setup for Kiro with AgentK's Meko: its MCP server and key in Kiro's MCP config, and
 * its agent with the capture hooks as Kiro's default (which Kiro sessions' `agentk` agent is built
 * from). AgentK's own config is left as it is: Kiro keeps its key in its own file.
 */
export async function setUpKiroMeko(core: Core, tools: MekoTools): Promise<void> {
  const meko = resolveMeko(core.config);
  if (!meko.apiKey) throw new HttpError(400, NO_MEKO);
  const tail: string[] = [];
  try {
    await tools.setUpKiro(meko.url, meko.apiKey, (line) => {
      core.log.info(`meko setup (kiro): ${line}`);
      tail.push(line);
      if (tail.length > 5) tail.shift();
    }, core.mekoHome());
  } catch (e) {
    const why = errMsg(e).replace(/([^.!?])$/, '$1.');
    throw new HttpError(502, `${why}${tail.length ? ` Last output: ${tail.join(' / ')}` : ''}`);
  }
  core.log.info(`set up Meko for Kiro (${meko.url})`);
}
