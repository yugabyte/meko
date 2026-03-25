---
title: Use Meko in Your IDE
linkTitle: Meko in Your IDE
description: Store personal preferences, coding standards, and team policies across sessions
weight: 2010
---

Meko gives your IDE's AI assistant persistent memory that follows you across sessions and projects. This example shows how to use Meko for storing and recalling development preferences.

## Use Cases

### Personal Preferences

Store your individual coding style and tool preferences:

```text
Remember that I prefer Python type hints on all function signatures
```

```text
Remember that I use pytest with fixtures for all testing
```

```text
Remember that I prefer 4-space indentation and single quotes in Python
```

In a new session, when you ask the agent to write code, it will recall your preferences and apply them automatically.

### Portable Developer Coding Preferences

Your preferences travel with you across projects. Create a datapack that stores your coding standards, then connect to it from any project:

```json
{
  "mcpServers": {
    "my-prefs": {
      "command": "meko",
      "args": ["mcp", "serve", "--name", "my-prefs"]
    }
  }
}
```

### Group/Team Policies and Standards

Create a shared datapack for your team's coding standards, testing policies, and architectural norms:

```text
Remember our team coding standard: all API endpoints must return JSON with a consistent error format
```

```text
Remember our testing policy: minimum 80% code coverage, integration tests for all API endpoints
```

Multiple team members can connect to the same datapack, ensuring consistent standards across the team.

### Project Storage

Use a dedicated datapack per project to store project-specific context:

- Architecture decisions
- API contracts
- Dependency choices and their rationale
- Known issues and workarounds

<!-- TODO: Add full walkthroughs with step-by-step IDE screenshots (Heather/Naveen) -->
<!-- TODO: Add complete worked example showing a developer setting up preferences and using them across multiple projects -->

## Next

- **[Build Software with Meko](../build-software-with-meko/)** --- Use conversation history and resumability
