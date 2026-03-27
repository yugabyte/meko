---
title: Meko in your development process
linkTitle: In your development process
description: Store personal preferences, coding standards, and team policies across sessions
weight: 2010
---
Meko gives your IDE's AI assistant persistent memory that follows you across sessions and projects. This example shows how to use Meko for storing and recalling development preferences.

## Use cases

### Tool-independent dev preferences & standards

*Use best-of-breed tools with consistent templates & standards.* 

Use Cursor to write code, Claude to review it, and Gemini to write the test plan... while following consistent coding conventions, document templates (for PRDs, specs, review feedback, test plans), and shared project knowledge.   

**Motivations** 
* Play to each tools' strengths or 
* Reuse existing tooling that's locked to an LLM vendor
... while maintaining consistency & portability.

**Detailed example**

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

In a new tool, new session, or new phase of the engineering lifecycle, all such preferences will be remembered and applied automatically.

### Portable developer coding preferences

*Take coding preferences with you across sessions, projects, and teams.*

Have you ever created a coding practice or pattern, only to have to re-teach it to your AI agent or coding tool on the next project, or even in the next session?

While AI dev tools do have mechanisms for memory and skills, do they apply at the session, project, multi-project, or team level?   Do they overlap and overlay?   Which take precedence?   Cursor does this one way, while Anthropic Claude does it a different way.  Surely, there must be an end to this madness.

Meko solves this.  Meko enables you to define clear and customizable scoping and hierarchy for memory... and it works portably across projects and tools.
### Group/team policies and standards

*Share a datapack across your team to enforce consistent coding standards and testing policies.*

**Detailed Example**

Create a shared datapack for your team's coding standards, testing policies, and architectural norms:

```text
Remember our team coding standard: all API endpoints must return JSON with a consistent error format
```

```text
Remember our testing policy: minimum 80% code coverage, integration tests for all API endpoints
```

Multiple team members can connect to the same datapack, ensuring consistent standards across the team.

### Project storage

*Store project-specific context like architecture decisions, API contracts, and known workarounds.*

Use a dedicated datapack per project to store project-specific context:

- Architecture decisions
- API contracts
- Dependency choices and their rationale
- Known issues and workarounds

<!-- TODO: Add full walkthroughs with step-by-step IDE screenshots (Heather/Naveen) -->
<!-- TODO: Add complete worked example showing a developer setting up preferences and using them across multiple projects -->

## Next

- [Meko in your agentic application](../build-software-with-meko/) - Use conversation history and resumability
