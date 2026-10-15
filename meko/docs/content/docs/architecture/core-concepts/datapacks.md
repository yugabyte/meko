---
title: Datapacks
linkTitle: Datapacks
description: The isolation unit for agent data in Meko
weight: 10
---

A **datapack** is Meko's fundamental unit of isolation. Each datapack provides a self-contained persistent store for conversations, memory, and knowledge. MCP clients connect to the shared endpoint (`https://mcp.mekodata.ai/mcp`) and target a datapack by passing the datapack ID in tool calls.

## What's in a datapack

When you create a datapack, Meko provisions isolated storage, scoped to your agent(s), and managed internally by Meko.

To connect an external MCP client (Cursor, Claude Code, and so on), you need:

- The **MCP server URL**, `https://mcp.mekodata.ai/mcp` (the same for every datapack and user).
- A **user API key**: a long-lived key you generate under **Settings > API Keys** in the portal (values start with `mko_tkn_`).

Agents select the datapack to connect to by passing the `datapack_id` inside each MCP tool call, not by using a different URL per datapack.

## Create a datapack

The first time you sign in to the Meko portal, Meko walks you through the steps to create your default datapack (`meko_default_datapack`) and connect it to your client.

To create additional datapacks:

1. In the Meko portal, navigate to **Datapacks**.
1. Click **Add Datapack**.
1. Follow the instructions in the configure wizard.

If Meko can't finish setting up the datapack's tracing project, creation fails with `Could not finish setting up the datapack; nothing was created. Please retry.` Nothing is left behind, so retry with the same name.

You can create more than one datapack at the same time, up to your plan's datapack cap.

> [!NOTE] Free tier
>
> Free accounts can create up to **10 datapacks**. When you reach the limit, **Add Datapack** is disabled. See [Reach a tier limit](#reach-a-tier-limit).

## Reach a tier limit

The Free tier includes limits on datapack creation, knowledge base storage, number of conversations, and WorkbenchLM token usage. When you exceed a limit, Meko displays a corresponding message. Click **Upgrade to Standard** on the message banner to open **Settings > Pricing** with the Standard plan selected.

Choose **AWS Marketplace** under Standard ($25 / month), then **Continue to AWS Marketplace**. After you subscribe, click **Set up your account**. If you skip that step, the plan stays on Free. Stripe is marked coming soon. See [Meko pricing](https://mekodata.ai/pricing/).

You cannot upgrade from Standard using the Marketplace. To upgrade, choose **Talk to our team**.

Free tier has the following limits:

- 10 datapacks
- 100 MB of knowledge base storage
- 1,000 conversations
- 10 million WorkbenchLM tokens

<!-- Using the CLI:

```bash
meko datapack create --name my-datapack
```

This returns the datapack's MCP server URL and API key:

```json
{
  "datapack_id": "e02a7107-c79e-451d-b360-9cd1e13f175d",
  "agent_id": "18311fce-36c5-40fe-bd45-67f5913b0b52",
  "name": "my-datapack",
  "mcp_server": {
    "url": "https://mekodev.com/<account_id>/mcp",
    "api_key": "318ce1c589d08edb11e24b0a781fe675"
  }
}
``` -->

## Group datapacks in a project

A project groups datapacks that work toward one outcome. It gives related
datapacks a visible structure without combining them into one store. Each
datapack remains an isolated home for its conversations, memory, and knowledge.
Deleting the project deletes those datapacks with it, along with their memories,
knowledge, and history.

Use folders to keep datapacks that do similar work together. The structure has
three levels:

- A project represents the outcome.
- A folder represents one kind or stage of work.
- A datapack holds the focused context for that work.

A project holds folders, and a folder holds datapacks. Folders do not nest. A
datapack can belong to only one project. Datapacks that are not in a project
remain **Unfiled** and continue to work normally.

Open **Projects** in the sidebar and click **New Project**. Enter a **Name**. **Description** is optional. Meko selects a **Layout** for you. **Create project** stays disabled until the name is filled in. If that layout starts empty, it also stays disabled until you have named the first folder.

Open the project and choose **Folders**. Click **Add folder**. Enter a **Folder name**. The **Add folder** button in the dialog stays disabled until the name is filled in. In a folder, click **Add a datapack**. On **Folders**, below the folder list, **What this project holds** shows the project's totals. The **learnings** row shows memories from the datapacks in the project, including ones still waiting for a promotion decision.

Choose **Activity** to see files uploaded, memories promoted, folders created, and datapacks filed.

You can also file a datapack from its own page. Open the actions menu (the gear next to **Share**) and choose **Add to project**. If the datapack is already in a project, the item is **Move to project**.

When you share a datapack, the recipient finds its project on **Projects**, under **Shared with me**. They see that datapack and the folder it sits in. The owner's other folders and datapacks in the same project stay hidden. **Add folder**, **Add a datapack**, and **Settings** stay with the project owner.

If you own the project, open **Settings** to delete it. The confirmation says the project, its folder structure, and every datapack filed inside it are deleted for good, along with their memories, knowledge and history. Deleting a folder is separate. The folder is removed, and the datapacks inside it become **Unfiled**.

## Share a datapack

To share your datapack with colleagues:

1. In the Meko portal, under **Datapacks**, select the datapack and click **Share**.
1. Stay on **Invite humans**.
1. Provide one or more **Email addresses** separated by commas, spaces, semicolons, or new lines, or click **Upload CSV**. 

    A CSV can include a header row where one column is an email column. A file Meko cannot read as text shows **Could not read file**. Export it as CSV and upload it again.

1. Choose a **Role for everyone in this list**: **Viewer**, **Contributor**, or **Maintainer**. The role applies to every address in the batch.
1. Click **Send invite**. With more than one address, the button shows the count, such as **Send 2 invites**.

One batch accepts up to 50 addresses; Meko displays an error if you exceed the limit. To invite more than 50, split the job into multiple batches.

Meko reports a result for each address. A row with no valid email is skipped. Inviting someone who already has access does nothing.

Pending invites are listed under **Pending**, where you can resend or revoke invites that have not been accepted.

### View members and change roles

1. In the Meko portal, under **Datapacks**, select the datapack and click **Share**.

    Members are listed under **Members**. You can search by name, email address, domain, or role. You can also filter by role.

1. If you are an Owner, assign roles to members by choosing a role in the role dropdown menu for the member.

### What a viewer can do

A **Viewer** can read the datapack.

In the portal and the REST API, a viewer sees the knowledge base, collective memory, conversations, and memories.

A viewer does not see who started a conversation, who added a memory, who uploaded someone else's file, or who promoted a memory.

A viewer does not see the member list.

Through MCP, a viewer can search the datapack.

Creating or changing a conversation or memory returns `read_only_access`.

A viewer can't upload artifacts or knowledge-base files.

A viewer can still delete their own memories, conversations they started, and knowledge-base files they uploaded.

A member moved down to **Viewer** can remove what they added before.

## Delete a datapack

Only the owner can delete a datapack.

Open the actions menu (the gear next to **Share**) and choose **Delete** plus the datapack name, such as **Delete research**. In the **Delete datapack** dialog, click **Delete**.

On a datapack's own menu, **Delete** does not open the dialog for `meko_default_datapack`, the datapack Meko created when you signed in.

You can delete a datapack when tracing is unavailable.

## Isolation model

Each datapack is fully isolated:

- **Data isolation**. Logical isolation within a shared YugabyteDB universe. An authenticated user's clients can search that user's memories across agent namespaces, but can't read another datapack member's private memories.
- **Credential isolation**. User API keys and database credentials are scoped to your account; datapack data is separated in storage.
- **MCP isolation**. Tool calls include a `datapack_id` so the shared MCP server only reads and writes that datapack's data.

## Connect via MCP

Each datapack exposes an MCP server that agents use to interact with their data.

When you create a datapack in the Meko portal, Meko provides instructions to connect your datapack to your agent automatically. The connection commands are also available under **Connect** in the Meko portal.

You can also configure clients by manually adding this configuration to your coding agent or agent framework. See [Integrations](/integrations/).

## Automatic capture and datapack pinning

When you have multiple datapacks, you designate one as your **active pin** - the default datapack where Meko saves conversations and memories. If you only have one datapack, Meko uses it automatically.

### How to change your pin

**Ask your agent in plain language:**

```text
Switch my Meko datapack to research
```

**Or invoke the selector skill directly:**

```text
# Claude Code
/meko-agent-skills:meko-select-datapack

# Claude Desktop
/meko-select-datapack-desktop
```

The interactive selector will:

- List your accessible datapacks with your access role (such as, `owner`, `maintainer`)
- Let you pick by row number or datapack name (partial names work)
- Show your current pin with a checkmark
- Navigate multiple pages if you have many datapacks

To clear your pin and return to your account default, reply with `clear` or `none`.

### Pins apply to new conversations only

**When you switch pins:** Your current conversation stays in its original datapack. The new pin takes effect only when you start a new chat. Existing conversations never move between datapacks.

| Scenario | Result |
|----------|--------|
| Pin a datapack, then start a new chat | New chat uses the pinned datapack |
| Pin a datapack while chatting | Current conversation stays in its original datapack; next chat uses the new pin |
| Switch pins on Claude Desktop | Ask the agent to "start a new Meko conversation" to apply the pin immediately |

**Example:** You're chatting in the "research" datapack. You say "Switch my pin to personal-projects." Meko updates the pin, but the current conversation keeps writing to "research." Your next chat will use "personal-projects."

### How the pin persists across sessions

**Claude Code**: The pin is stored per project and survives restarting Claude Code, clearing your transcript, and opening another window in the same repository. Different projects can have different pins.

**Claude Desktop**: The pin is stored in Meko memory and survives across all Desktop conversations until you change or clear it.

If your pinned datapack is deleted or you lose access, Meko fails gracefully rather than silently writing elsewhere. Run the selector to pick an accessible datapack, or clear the pin.

> [!NOTE] Cursor, Codex, and Kiro
>
> These clients don't have a guided datapack selector. You can still name a datapack directly in a prompt without changing your pin (for example, "save this conversation to the research datapack"). To set or change your default pin, use Claude Code or Claude Desktop's selector.

## Manage datapacks

Manage your datapacks from the **Datapacks** page in the Meko portal. **Datapacks** lists all your datapacks. From here you can add and share datapacks.

Select a datapack to view the conversations, memories, learnings, and knowledge the datapack holds.

From here, you can review and promote learnings to [knowledge](../knowledgebase/).

<!--
| Command | Description |
| :------ | :---------- |
| `meko datapack list` | List all your datapacks |
| `meko datapack status --name <n>` | View datapack summary |
| `meko datapack delete --name <n>` | Permanently delete a datapack |

See the [CLI reference](../../../reference/meko-cli/) for full details.
-->
