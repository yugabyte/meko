import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { HARNESS_IDS } from '../../src/shared/harnesses.js';
import { TOOL_DEFS } from '../../src/server/tooldefs.js';
import { getPaths } from '../../src/server/paths.js';
import { loaderFlags, skillPath, toolsMcpConfigPath, toolsPluginDir, toolsReady, writeToolFiles } from '../../src/server/tools.js';
import { tempDir } from '../helpers.js';

describe("the session tools' files", () => {
  it('names me_koder mcp for Claude Code, and puts the one skill in its plugin and where Kiro and OpenCode read it', () => {
    const paths = getPaths(join(tempDir('akl-tools-'), 'ak'));
    expect(toolsReady(paths, true)).toBe(false);
    expect(writeToolFiles(paths)).toBe(true);
    const server = JSON.parse(readFileSync(toolsMcpConfigPath(paths), 'utf8')).mcpServers.agentk_sessions;
    expect(server).toMatchObject({ type: 'stdio', command: process.execPath });
    expect(server.args.at(-1)).toMatch(/mcpmain\.ts$/);
    const skill = readFileSync(join(toolsPluginDir(paths), 'skills', 'sessions', 'SKILL.md'), 'utf8');
    expect(skill).toMatch(/^---\nname: agentk-sessions\n/);
    expect(readFileSync(skillPath(paths), 'utf8')).toBe(skill);
    expect(JSON.parse(readFileSync(join(toolsPluginDir(paths), '.claude-plugin', 'plugin.json'), 'utf8')).name).toBe('agentk-sessions');
    expect([toolsReady(paths, true), toolsReady(paths, false)]).toEqual([true, false]);
  });

  it('removes what an earlier start wrote when the skill is missing from the install, so sessions start without the tools', () => {
    const paths = getPaths(join(tempDir('akl-tools-'), 'ak'));
    writeToolFiles(paths);
    expect(writeToolFiles(paths, join(paths.home, 'nope', 'SKILL.md'))).toBe(false);
    expect([existsSync(toolsMcpConfigPath(paths)), existsSync(skillPath(paths)), toolsReady(paths, true)]).toEqual([false, false, false]);
  });

  it.each([
    [['--require', '/tsx/preflight.cjs', '--import', 'file:///tsx/loader.mjs'], ['--require', '/tsx/preflight.cjs', '--import', 'file:///tsx/loader.mjs']],
    [['--import=tsx', '--eval', 'console.log(1)', '--watch'], ['--import=tsx']],
    [[], []],
  ])('starts it in a dev server with only the loader flags of %j', (argv, want) => {
    expect(loaderFlags(argv)).toEqual(want);
  });
});

describe("start_sessions' agent", () => {
  it('offers every harness AgentK runs (tooldefs.ts imports nothing, so it lists them itself)', () => {
    const start = TOOL_DEFS.find((t) => t.name === 'start_sessions')!;
    const item = (start.inputSchema.properties as { sessions: { items: { properties: { agent: { enum: string[] } } } } }).sessions.items;
    expect(item.properties.agent.enum).toEqual([...HARNESS_IDS]);
  });
});
