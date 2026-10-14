---
title: Connect to an AI agent
linkTitle: AI agents
description: Integrate Meko with Cursor, Claude Desktop, Claude Code, Codex, Kiro, OpenCode, and more
weight: 3010
type: docs
---

Integrate with AI agents for interactive use cases, such as saving personal, team, and organizational preferences, standards, and policies.

> [!NOTE] Free tier
>
> Every MCP call from an external client counts toward your account's Free tier quotas (conversations, retrievals, and related caps). See [Meko pricing](https://mekodata.ai/pricing/) for plan details.

## Before you connect

Every client uses the MCP server URL `https://mcp.mekodata.ai/mcp`. The **Datapacks > Connect** dialog also shows this URL.

The automated installer requires Node.js 20.18.1 or newer.

Choose one authentication method for each client:

- **API key:** In the portal, open **Settings > API Keys > Generate API key**. Name the key, then copy or download it before you close the dialog. The key starts with `mko_tkn_`, and Meko shows it only once. In the instructions below, replace `<paste-your-api-key>` with this key.
- **OAuth:** Add the MCP server URL without an API key or authentication headers. The client opens your browser so you can sign in to the Meko portal. OAuth works only when the client registers a callback that Meko accepts. Use an API key for command-line clients and Kiro.

Use an API key for command-line clients, CI, and scripts. Command-line OAuth uses a local callback that Meko currently rejects, and unattended clients can't open a browser when an OAuth session expires.

Don't configure both methods for the same client. The client may choose OAuth and ignore your API key.

For the full workflow, see [MCP server URL](../#mcp-server-url) and [Choose an authentication method](../#choose-an-authentication-method).

### OAuth and Manual setup

When you connect with OAuth or using the Manual setup, your agent interacts with Meko _on demand_ only: you need to mention "Meko" in your prompt (for example, _"save this to Meko,"_ _"search my Meko knowledge base"_) for the agent to call the tools. The agent won't proactively save conversations or capture memories _unless you ask._

To get automatic behavior, use the **Automated setup** instead, or write your own skills to teach your agent when to call Meko tools. Claude Desktop has no session hooks, so its automatic capture still depends on the model. See [Claude Desktop](#claude-desktop).

## Cursor Desktop

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

##### OAuth

In Cursor Desktop:

1. Open **Cursor > Settings > Cursor Settings > Tools & MCPs > New MCP Server**.
1. Add to `mcp.json` without any auth headers:

    ```json
    {
      "mcpServers": {
        "meko": {
          "url": "https://mcp.mekodata.ai/mcp"
        }
      }
    }
    ```

1. Complete the OAuth handshake when the browser opens.

##### Manual setup

In Cursor Desktop:

1. Choose **Cursor>Settings>Cursor Settings>Tools & MCPs>New MCP Server**.

1. Modify the `mcp.json` file that Cursor opens to add the following MCP server configuration.

    ```json
    {
      "mcpServers": {
        "meko": {
          "url": "https://mcp.mekodata.ai/mcp",
          "headers": {
            "Authorization": "Bearer <paste-your-api-key>",
            "User-Agent": "meko-mcp-installer/1.0"
          }
        }
      }
    }
    ```

    Replace `<paste-your-api-key>` with the API key you saved from **Settings>API Keys**. Keep the `Bearer ` prefix, and keep the `User-Agent` header — see [Choose an authentication method](../#choose-an-authentication-method).

    The result should look like the following:

    ![Cursor Desktop](/images/integrations-cursor-settings.png)

1. Verify the Connection by entering the following in chat.

    ```text
    List my datapacks in Meko
    ```

## Cursor Web

> [!NOTE] Coming soon
>
> This section is under development. Check back soon.

## Claude Desktop

Pick a method to connect the MCP server.

| Install Method | Use when | Builds skills files |
| --- | --- | --- |
| 1. Automated installer | You want Claude to follow Meko guidance | Yes<br>You must still import them |
| 2. Desktop extension | You want the MCP server without a terminal | No |
| 3. OAuth | You want OAuth, or you want to edit the config yourself | No |
| 4. Manual | You want to edit the config yourself | No |

Only the automated installer builds the Desktop skills. The skills are `meko-mcp-tools-desktop` and `meko-select-datapack-desktop`; the second skill keeps a pinned datapack across chats. They tell Claude when to call Meko tools. Without them, Claude calls Meko only when you ask.

> [!NOTE] Automatic capture
>
> Claude Desktop has no session hooks, so even with the skills installed, automatic capture depends on the model you are using. If Claude recalls memories but doesn't save new ones, see [Troubleshooting](../../troubleshooting/#common-problems-installing-and-connecting).

##### Automated setup

Remove the Desktop extension in **Settings > Extensions** before you run the installer. Otherwise you can end up with the extension and a second MCP server entry.

The installer asks before it replaces an entry it finds in `claude_desktop_config.json`, but it doesn't remove the extension.

To run the automated setup:

1. In the Meko portal, select **Connect** and follow the steps.

    Alternately, run the following command:

    ```sh
    npx @yugabytedb/meko-mcp@latest --client claude-desktop --url "https://mcp.mekodata.ai/mcp" --api-key <paste-your-api-key>
    ```

    Replace `<paste-your-api-key>` with the key you saved from **Settings > API Keys**.

    The installer writes `mcpServers.meko` in `claude_desktop_config.json`, builds two `.skill` files, `meko-mcp-tools-desktop.skill` and `meko-select-datapack-desktop.skill`, and prints a path for each on a line that starts with `Built Meko skill bundle:`.
    
    Claude Desktop has no API the installer can use to import skills, so you must import both files.

1. Import the skills files into Claude Desktop by dragging each file onto the Claude Desktop window, or using **Settings > Skills**.
1. Quit and restart Claude Desktop.
1. Ask Claude:

    ```text
    Which Meko skills do you have available?
    ```

    The reply should name `meko-mcp-tools-desktop` and `meko-select-datapack-desktop`.

When upgrading the installer, run the installer again and import the new `.skill` files. Desktop keeps using the copies you already imported until you replace them.

##### Desktop extension

The `.mcpb` extension installs the MCP server only. It doesn't install the skills.

You need Node.js 20.18.1 or newer on your `PATH`. The extension runs `npx mcp-remote`.

1. Download [meko-2.3.0.mcpb](/downloads/meko-2.3.0.mcpb). This is the Desktop extension for installer 2.3.0, the same version as `npx @yugabytedb/meko-mcp@2.3.0`.
1. In Claude Desktop, open **Settings > Extensions > Install from file** and select the file. You can also drag the file onto that pane.
1. When prompted, paste `https://mcp.mekodata.ai/mcp` and your API key.
1. Quit and restart Claude Desktop.
1. Verify the connection:

    ```text
    List my datapacks in Meko
    ```

##### OAuth

In Claude Desktop:

1. Navigate to **Settings**, and under **Customize** select **Connectors**.
1. Search for **Meko** in the Connectors search box, and add it as a custom MCP connector with the URL `https://mcp.mekodata.ai/mcp`.
1. Complete the OAuth handshake when the browser opens.

##### Manual setup

In Claude Desktop (whether used for Chat, Cowork, or Code):

1. Navigate to **Settings**, and under **Desktop app** select **Developer**, then click **Edit Config**.
1. Edit the `claude_desktop_config.json` file to add the following MCP Server entry. Replace `<paste-your-api-key>` with the token you saved.

    ```json
    {
      "mcpServers": {
        "meko": {
          "command": "npx",
          "args": [
            "-y",
            "mcp-remote@0.1.38",
            "https://mcp.mekodata.ai/mcp",
            "--transport",
            "http-only",
            "--header",
            "Authorization:${AUTH_HEADER}",
            "--header",
            "User-Agent:meko-mcp-installer/1.0"
          ],
          "env": {
            "AUTH_HEADER": "Bearer <paste-your-api-key>"
          }
        }
      }
    }
    ```

    **Why split the key between `args` and `env`:** The `Bearer <key>` header
    value contains a space. Environment variable values can safely contain
    spaces, but some clients mishandle spaces in `args` entries. The
    configuration keeps the argument space-free as
    `Authorization:${AUTH_HEADER}` and stores `Bearer <key>` in `env`. This is
    the pattern `mcp-remote` expects.

    **How the substitution works:** Claude Desktop doesn't expand
    `${AUTH_HEADER}`. It passes the `args` array as written and sets
    `AUTH_HEADER` in the `mcp-remote` process environment. When `mcp-remote`
    parses the `Authorization` header, it replaces `${AUTH_HEADER}` with the
    `Bearer <key>` value.

    **Why the name matters:** `mcp-remote` substitutes only environment
    variables that are explicitly referenced in `args`. The `AUTH_HEADER` name
    in `env` must match `${AUTH_HEADER}` in `args`. A different or unreferenced
    variable name doesn't authenticate the connection.

1. Quit and restart Claude Desktop.

    The result should look like the following:

    ![Claude Desktop](/images/integrations-claude-desktop.png)

    > [!TIP] Handling errors
    >
    > If you see errors when restarting Claude, examine the log file that appears. Verify that you have the correct version of node.js binaries in your path, use absolute paths, and consult online MCP Server configuration troubleshooting guides.

1. Verify the connection by entering the following in chat:

    ```text
    List my datapacks in Meko
    ```

### Uninstall

How you remove Meko from Claude Desktop depends on how you installed it, because each installation method creates a different artifact.

##### Automated setup

Run the following:

```bash
npx @yugabytedb/meko-mcp@latest --client claude-desktop --uninstall
```

If you installed under a non-default server name, pass `--server-name <name>`. The uninstall removes only that MCP server entry. If you imported the skill files, remove `meko-mcp-tools-desktop` and `meko-select-datapack-desktop` in **Settings > Skills**.

##### Desktop extension

In Claude Desktop, open **Settings > Extensions** and remove the Meko extension. If you imported the skill files, remove them in **Settings > Skills**.

##### OAuth

In Claude Desktop:

1. Navigate to **Settings**, and under **Customize** select **Connectors**.
1. Search for **Meko** in the Connectors search box and delete it.

##### Manual setup

In Claude Desktop:

1. Navigate to **Settings**, and under **Desktop app** select **Developer**, then click **Edit Config**.
1. Edit the `claude_desktop_config.json` file to delete the `mcpServers.meko` entry.
1. Restart Claude Desktop.

## Claude Code

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

##### OAuth

1. Run the setup command in your shell to add the Meko MCP server:

    ```sh
    claude mcp add --scope user --transport http meko https://mcp.mekodata.ai/mcp
    ```

1. Complete the OAuth handshake when the browser opens.

##### Manual setup

1. Run the setup command in your shell to add the Meko MCP server to Claude Code:

    ```sh
    claude mcp add --scope user --transport http meko https://mcp.mekodata.ai/mcp \
      --header "Authorization: Bearer <paste-your-api-key>" \
      --header "User-Agent: meko-mcp-installer/1.0"
    ```

    Replace `<paste-your-api-key>` with the API key you saved from **Settings>API Keys**. Keep the `Bearer ` prefix — see [Choose an authentication method](../#choose-an-authentication-method).

1. Verify the connection by entering the following at the command-line console:

    ```txt
    List my datapacks in Meko
    ```

### Updating the skills plugin

Skill content (the behavioral guides that teach the agent how to use Meko's MCP tools) ships in the `meko-agent-skills` plugin and versions independently of the MCP server. The marketplace source is `https://github.com/yugabyte/meko-skills.git`.

##### Inside an active Claude Code session

```text
/plugin marketplace update meko-agent-skills
/plugin update meko-agent-skills@meko-agent-skills
/reload-plugins
```

If `/reload-plugins` warns that it would re-read the conversation, rerun it as `/reload-plugins --force`. Then run `/doctor` to confirm there are no plugin load errors.

##### From the shell

```sh
claude plugin marketplace update meko-agent-skills
claude plugin update meko-agent-skills@meko-agent-skills
```

The qualified `plugin@marketplace` form is required. Re-running the installer (`npx @yugabytedb/meko-mcp@latest`) updates an existing install automatically.

##### Enable auto-update

Auto-update is per-marketplace and **disabled by default** for third-party marketplaces. To opt in:

1. Open `/plugin`, switch to the **Marketplaces** tab, select `meko-agent-skills`, and toggle **Enable auto-update**.
1. Or add `"autoUpdate": true` to the existing `meko-agent-skills` entry in `~/.claude/settings.json` (user scope) or a project's `.claude/settings.json`. Keep the `source` field — an `autoUpdate`-only replacement drops the marketplace source:

    ```json
    {
      "extraKnownMarketplaces": {
        "meko-agent-skills": {
          "source": {
            "source": "git",
            "url": "https://github.com/yugabyte/meko-skills.git"
          },
          "autoUpdate": true
        }
      }
    }
    ```

Once enabled, Claude Code refreshes the marketplace and updates installed plugins in the background after startup. The running session keeps the versions it loaded at launch; you'll be prompted to `/reload-plugins`, or the new versions load on the next launch.

## Open AI Codex Desktop

##### Automated setup

In the Meko portal, select **Connect** and follow the steps.

When you provide an API key on macOS, the installer adds `MEKO_API_KEY` to the launchd environment used by apps opened from Spotlight or Alfred. Quit every running Codex process and relaunch Codex after installation. A running process keeps the environment it had when it started.

##### OAuth

In Codex Desktop:

1. Choose **Codex>Settings>MCP servers>Add server>Streamable HTTP**.
1. Enter the URL `https://mcp.mekodata.ai/mcp` and leave the Bearer token field empty.
1. Click **Authenticate** beside Meko in the list of MCP servers and complete the OAuth handshake when the browser opens.

##### Manual setup

In Codex Desktop:

1. Choose **Codex>Settings>MCP servers>Add server>Streamable HTTP**.
1. Enter a name for the Meko MCP server.
1. Enter the Meko MCP server URL (`https://mcp.mekodata.ai/mcp`).
1. Enter a name (such as `MEKO_MCP_TOKEN`) for the Bearer token environment variable, and click **Save**.

    The result should look like the following:

    ![Codex Desktop](/images/integrations-codex-desktop.png)

1. Set the `MEKO_MCP_TOKEN` environment variable with your Meko API key in your terminal using the following command:

    ```sh
    launchctl setenv MEKO_MCP_TOKEN <paste-your-api-key>
    ```

1. Verify that you set `MEKO_MCP_TOKEN` correctly with your API key.

    ```sh
    launchctl getenv MEKO_MCP_TOKEN
    ```

1. Quit and restart Codex Desktop.

1. Verify the connection by entering the following in chat:

    ```text
    List my datapacks in Meko
    ```

## Kiro IDE and CLI

Kiro IDE and `kiro-cli` share their MCP configuration, skills, and hooks. One install configures both:

```bash
npx @yugabytedb/meko-mcp@latest --client kiro
```

The `kiro-cli` and `kiro-ide` aliases run the same install. If you set `$KIRO_HOME`, the installer uses that directory instead of `~/.kiro`.

The installer uses API-key authentication because Kiro registers a local OAuth callback that Meko rejects. It also installs the Meko skill, steering, and both Kiro hook formats for durable capture.

### Manual setup

If you only need an on-demand MCP connection, add this entry to `~/.kiro/settings/mcp.json`:

```json
{
  "mcpServers": {
    "meko": {
      "url": "https://mcp.mekodata.ai/mcp",
      "headers": {
        "Authorization": "Bearer <paste-your-api-key>",
        "User-Agent": "meko-mcp-installer/1.0"
      }
    }
  }
}
```

Replace `<paste-your-api-key>` with the key you saved from **Settings > API Keys**, then protect the file because it contains the key:

```bash
chmod 600 ~/.kiro/settings/mcp.json
```

Manual setup doesn't install the Meko skill, steering, or capture hooks. Use the automated installer when you want Meko to capture conversations and apply its behavioral guidance.

Kiro hooks expose user prompts and final assistant responses. They don't expose tool calls or hidden reasoning, so those details aren't included in automatic capture.

### Uninstall

```bash
npx @yugabytedb/meko-mcp@latest --client kiro --uninstall
```

## OpenCode CLI and TUI

The installer supports OpenCode CLI and TUI 1.18.x. Choose one installation scope:

- **User scope** makes Meko available in every OpenCode project.
- **Project scope** installs Meko only in the current project.

Use only one scope for a project.

##### User scope

```bash
npx @yugabytedb/meko-mcp@latest --client opencode --scope user
```

##### Project scope

Run this command from the project root:

```bash
npx @yugabytedb/meko-mcp@latest --client opencode --scope project
```

The installer prompts for your Meko API key, installs the Meko skill, and adds an installer-owned plugin. The plugin supplies the MCP configuration when OpenCode starts, so the installer doesn't rewrite `opencode.json` or `opencode.jsonc`. It stores the API key in a private file with mode `0600`.

Restart OpenCode after installation, then verify the connection:

```text
List my datapacks in Meko
```

OpenCode automatically captures visible user text and the first visible assistant message for each completed turn. It doesn't capture tool calls, hidden reasoning, files, attachments, or streaming output. Check queued work with:

```bash
npx @yugabytedb/meko-mcp@latest --capture-status
```

To preview and import existing local OpenCode sessions:

```bash
npx @yugabytedb/meko-mcp@latest --migrate --dry-run
npx @yugabytedb/meko-mcp@latest --migrate
```

Migration imports local root sessions returned by the OpenCode CLI. OpenCode 1.18.x doesn't expose child sessions through that interface. Installer 2.3.0 doesn't support OpenCode Desktop, V2 preview APIs, or headless or remote sessions.

### Uninstall

Uninstall using the same scope you used during installation. For user scope, run:

```bash
npx @yugabytedb/meko-mcp@latest --client opencode --scope user --uninstall
```

For a project installation, run the command from the project root and replace `--scope user` with `--scope project`. Uninstall removes installer-owned configuration and credentials. It keeps queued capture data so you can recover unsent exchanges.

## VS Code

1. Run the following command:

    ```sh
    echo "### Installing the latest Node.js ###"
    curl -o- https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.2/install.sh | bash
    source ~/.$(basename $SHELL)rc
    nvm install --lts
    echo "### Registering the Meko MCP server ###"
    npx mcp-add@latest \
      --name meko \
      --type http \
      --url 'https://mcp.mekodata.ai/mcp' \
      --headers 'Authorization=Bearer <paste-your-api-key>,User-Agent=meko-mcp-installer/1.0' \
      --scope global \
      --clients 'vscode'
    ```

    VS Code can connect to a remote MCP server on its own, so this registers the
    URL and its headers directly. Unlike Claude Desktop, it needs no local
    `mcp-remote` helper.

    Replace `<paste-your-api-key>` with the API key you saved from **Settings>API Keys**.

1. Verify the connection by entering the following in chat:

    ```text
    List my datapacks in Meko
    ```

<!-- ## Windsurf, GitHub Copilot CLI, Gemini Anti-Gravity, and others

For Windsurf, GitHub Copilot CLI, Gemini Anti-Gravity, Continue, Goose, and Windsurf, use the "Automated Setup" described in the [Quick Start](/quick-start/#connect-your-ai-client).

Additional support for the following is coming soon:
* OpenAI chatgpt Desktop
* Lovable
* Replit -->
