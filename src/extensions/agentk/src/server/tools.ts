import { randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { claudePath, type Paths } from './paths.js';
import { TOOL_DEFS, TOOLS_SERVER } from './tooldefs.js';

/**
 * Kode's session tools: the `kode_sessions` MCP server (`me_kode mcp`, mcp.ts) every harness's
 * sessions get, and the skill that says how to use it. Here are the files each harness launches with;
 * the server's side of a call (the hook socket's `/tools`) is `SessionTools` in actions.ts.
 */

/** The skill's one source: src/tools/skill in the repo, dist/tools/skill in a build. */
const SKILL_SOURCE = join(dirname(fileURLToPath(import.meta.url)), '..', 'tools', 'skill', 'SKILL.md');
/** The skill's name, in its front matter and as its folder. */
const SKILL = 'kode-sessions';

// ---- the files sessions launch with ----

/** Where the tools' files are written for sessions to load. */
export function toolsDir(paths: Paths): string {
  return join(paths.run, 'tools');
}

/** Claude Code's `--mcp-config` file, naming `me_kode mcp`. */
export function toolsMcpConfigPath(paths: Paths): string {
  return join(toolsDir(paths), 'mcp.json');
}

/** The plugin Claude Code sessions load for the skill (`--plugin-dir`): no hooks, only `skills/`. */
export function toolsPluginDir(paths: Paths): string {
  return join(toolsDir(paths), 'claude');
}

/** The folder of skills OpenCode sessions search (`skills.paths`), and where Kiro's `kode` agent names the skill (`skill://`). */
export function skillsDir(paths: Paths): string {
  return join(toolsDir(paths), 'skills');
}

/** The skill's copy in `skillsDir`. */
export function skillPath(paths: Paths): string {
  return join(skillsDir(paths), SKILL, 'SKILL.md');
}

/**
 * How a session's CLI starts `me_kode mcp`: its own entry (mcpmain.js), which loads only the MCP
 * server, not the CLI's imports of the whole server. An install runs it through `app/current`, which
 * follows updates, so an MCP server started after an update runs the version installed then.
 * Elsewhere (a dev server) it's this Node, with this process's loader flags, on this build's entry.
 */
export function toolsCommand(paths: Paths): { command: string; args: string[] } {
  const current = join(paths.app, 'current');
  const installed = join(current, 'lib', 'dist', 'server', 'mcpmain.js');
  // In their local spelling, as every path a session's CLI is given (claudePath).
  if (process.env.KODE_APP_DIR && existsSync(installed)) return { command: claudePath(join(current, 'node', 'bin', 'node')), args: [claudePath(installed)] };
  const here = dirname(fileURLToPath(import.meta.url));
  const entry = existsSync(join(here, 'mcpmain.js')) ? join(here, 'mcpmain.js') : join(here, 'mcpmain.ts');
  return { command: process.execPath, args: [...loaderFlags(process.execArgv), claudePath(entry)] };
}

/** Node's flags that load code (tsx's loader in a dev server), from this process's: not `--eval`, `--watch` and the like. */
export function loaderFlags(execArgv: string[]): string[] {
  const valued = new Set(['--require', '-r', '--import', '--loader', '--experimental-loader']);
  const out: string[] = [];
  for (let i = 0; i < execArgv.length; i++) {
    const a = execArgv[i];
    if (valued.has(a) && i + 1 < execArgv.length) out.push(a, execArgv[++i]);
    else if (valued.has(a.split('=')[0])) out.push(a);
  }
  return out;
}

/**
 * The tools in an OpenCode session's config (`OPENCODE_CONFIG_CONTENT`): the MCP server (OpenCode
 * passes it the session's environment), the skills folder, and an ask before each tool that changes
 * something (OpenCode allows every tool unless told otherwise; its MCP tools are `<server>_<tool>`).
 */
export function openCodeTools(paths: Paths): Record<string, unknown> {
  const { command, args } = toolsCommand(paths);
  return {
    mcp: { [TOOLS_SERVER]: { type: 'local', command: [command, ...args] } },
    skills: { paths: [claudePath(skillsDir(paths))] },
    permission: Object.fromEntries(TOOL_DEFS.filter((t) => !t.readOnly).map((t) => [`${TOOLS_SERVER}_${t.name}`, 'ask'])),
  };
}

/** Kiro's `mcpServers` entry for the tools. Kiro passes the session's environment to it (KODE_HOOK_SOCK, KODE_HOOK_TOKEN). */
export function kiroToolsServer(paths: Paths): Record<string, unknown> {
  // Starting sessions can clone a repo first: give a call as long as that takes.
  return { ...toolsCommand(paths), timeout: 600_000 };
}

/** Writes `text` to `file` through a temp file renamed into place, unless it's already there. */
function writeIfChanged(file: string, text: string): void {
  try {
    if (readFileSync(file, 'utf8') === text) return;
  } catch {
    /* not there yet */
  }
  mkdirSync(dirname(file), { recursive: true });
  const tmp = `${file}.${randomBytes(4).toString('hex')}.tmp`;
  writeFileSync(tmp, text);
  renameSync(tmp, file);
}

/**
 * Writes what sessions launch with: Claude Code's MCP config and skill plugin, and the skill's copy
 * Kiro and OpenCode read. Written at start whether or not the tools are on (the launch decides).
 * False when the skill's source is missing from this install: the copies an earlier start wrote are
 * removed, so sessions start without the tools (`toolsReady`) rather than with an old command.
 */
export function writeToolFiles(paths: Paths, source = SKILL_SOURCE): boolean {
  let skill: string;
  try {
    skill = readFileSync(source, 'utf8');
  } catch {
    rmSync(toolsDir(paths), { recursive: true, force: true });
    return false;
  }
  // Claude Code passes the session's environment to the server (KODE_HOOK_SOCK, KODE_HOOK_TOKEN), as Kiro does.
  writeIfChanged(toolsMcpConfigPath(paths), `${JSON.stringify({ mcpServers: { [TOOLS_SERVER]: { type: 'stdio', ...toolsCommand(paths) } } }, null, 2)}\n`);
  const plugin = toolsPluginDir(paths);
  writeIfChanged(join(plugin, '.claude-plugin', 'plugin.json'), `${JSON.stringify({
    name: SKILL, version: '1.0.0', description: "How to use Kode's session tools: open projects, start sessions, move them between stages.",
    author: { name: 'Kode' },
  }, null, 2)}\n`);
  writeIfChanged(join(plugin, 'skills', 'sessions', 'SKILL.md'), skill);
  writeIfChanged(skillPath(paths), skill);
  return true;
}

/** Whether sessions launch with the tools: turned on, and their files written. */
export function toolsReady(paths: Paths, enabled: boolean): boolean {
  return enabled && existsSync(toolsMcpConfigPath(paths)) && existsSync(skillPath(paths));
}
