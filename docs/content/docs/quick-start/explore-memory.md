---
title: Explore Memory
linkTitle: Explore Memory
description: See how to use Meko to store your memories
weight: 1010
---

Meko retains memory, based on what you have provided and on previous interactions.

## Your IDE without Meko

To show how this works, before even setting up Meko, in the chatbot of your AI-driven IDE or development tool, enter the following:

```text
Propose a 5 day vacation for me.
```

Without any stored memories, your agent either recommends a randomly-selected vacation, or perhaps asks generic follow-up questions such as "What kind of vacation?", "What budget?", or "To where?"

For example, Cursor responds with something similar to the following:

```text
Here’s a 5-day vacation you can use as-is or swap the city for another place you like.

## Option A: City + food (fits “you like ice cream”)

Where: Barcelona (or substitute: Lisbon, Rome, Tokyo, Mexico City)

<...>

---

## Option B: Nature reset

Where: Banff / Lake Louise (or substitute: Pacific Northwest coast, Swiss Alps, Costa Rica cloud forest)

<...>

---

## Quick choices so the plan fits you

If you answer these questions, the itinerary can be tightened (budget, pace, solo/couple/family):

1. Relax vs. busy (roughly 50/50 vs. mostly slow)?
2. Fly vs. drive from home?
3. Rough season (when you’re thinking of going)?
4. Beach, city, or mountains as a first pick?
```

## Connect Meko

Each Meko account has one or more Datapacks, which is a container for memory and knowledge. A default Datapack is created for you when you sign up.

To start using your Datapack, register the MCP Server associated with this Datapack with your AI chatbot-enabled IDE or development tool.

{{< tabpane text=true right=false >}}
{{% tab header="Cursor" lang="cursor" %}}

Add to your Cursor MCP settings (`.cursor/mcp.json`):

```json
{
  "mcpServers": {
    "my-datapack": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-datapack"]
    }
  }
}
```

{{% /tab %}}
{{% tab header="Claude Code" lang="claude" %}}

Add the MCP server configuration. The method depends on your Claude version.

**Developer Mode**

Go to **Claude>Settings>Developer>Edit Config**. Edit the JSON file and add the following:

```json
{
  "mcpServers": {
    "my-datapack": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-datapack"]
    }
  }
}
```

Then restart Claude.

{{% /tab %}}
{{% tab header="OpenAI Codex" lang="openai" %}}
Add the MCP server configuration to your Codex settings:

```json
{
  "mcpServers": {
    "my-datapack": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-datapack"]
    }
  }
}
```

{{% /tab %}}
{{< /tabpane >}}

For troubleshooting, see [Integrations](../integrations/_index)

## Your IDE with Meko

Now that you are connected, tell your AI about the new capabilities.

In your IDE or dev tool, enter the following:

```text
You have access to the Meko platform through MCP server.
Have Meko remember all my personal preferences.
```

<!-- TODO: validate the above.   Evaluate whether the prompt also needs this text:  "agent_id is cursor_agent and user_id is <your_username>" -->

If the connection is working, your IDE will use the Meko MCP tools to store the memory.

### Create a memory

Type the following to persist your personal preferences as memory:

```text
Please remember that I like to take vacations at tropical beach locations, on a budget
```

### Prompt with Meko

Start a new chat session, then type:

```text
Propose a 5 day vacation for me.
```

Your AI can now leverage the memory to provide a more relevant response.

For example, Cursor now responds with something similar to the following:

```text
Here’s a 5-day plan tuned to what you’ve saved: tropical beach + budget.

## Pick a base: Isla Holbox or Puerto Morelos area, Mexico (Yucatán)

Why it fits: Warm water, palm-lined beaches, easy buses from Cancún airport, hostels / small hotels and street food keep costs down. (Swap-ins: Koh Lanta, Thailand or Nusa Penida + Sanur, Bali if you’re already closer to Asia.)

<...>

---

## Budget levers

- Stay: hostel dorm or simple guesthouse slightly back from the sand (often half the price of beachfront).
- Eat: markets, loncherías, set “menu del día” lunches; limit restaurant dinners.
- Move: buses + shared shuttles; avoid private taxis except airport if needed.
- Play: prioritize free swimming, walking, and one paid half-day trip instead of stacked tours.

---

## If you tell me your month, home airport region, and solo vs. group, I can narrow this to one place, rough daily spend, and a rain-day backup.
```

Compare the response to the earlier response prior to connecting to and configuring Meko. The agent now retrieves your stored preferences and immediately proposes a vacation aligned to these preferences (a tropical beach vacation within budget). No follow-up questions were needed.

### Verify

You can verify what Meko stored by checking your datapack's memories in the Meko portal. You'll see:

- The stored memories with their extracted entities
- Key metrics: number of memories, knowledge base sources, and MCP requests

Type:

```text
What does Meko remember about me?
```

## Recap

We've seen how Meko uses _memory_ to enhance a user experience through _personalization_.

Memory from Meko can do much more. Beyond personal preferences, it can also capture personal, team, and organization preferences, standards, policies, and more. Meko's memory empowers teams to collaborate. Memories can be captured, shared, managed, and extended... and not just for human teams, but also for agent teams. Meko is a multi-agent data layer for shared memory.

Under the hood, memories are stored in a graph database... where the graph's entities and relationships are derived by Meko's processing of the LLM prompts.

## Next

[Explore Knowledge](../explore-knowledge/) to see Meko in action.
