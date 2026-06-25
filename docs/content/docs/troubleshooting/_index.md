---
title: Troubleshooting
linkTitle: Troubleshooting
description: Report issues, share feedback, and get help with Meko
weight: 6000
type: docs
---

## Common problems installing and connecting

If you have problems with your setup, check the following common issues:

- API key.

    Ensure you pasted your API key correctly (no whitespace). Ensure the key is not revoked or expired.

- Node version.

    Ensure you have the correct version of Node (Node.js 18.0 or newer) installed.

- Claude Desktop.
    - Restart is required after editing your configuration.
    - `--allow-http` flag is required in [manual setup](../../integrations/connect-to-ai-agent/#manual-setup-1).

- Agent answers but never calls Meko tools.

    The Meko skills are not installed; see **Connect>Manual MCP Install** in the portal.

    Note that if you installed Meko using OAuth alone to authenticate (that is, you didn't use the automatic installer), your agent interacts with Meko _on demand_ only: you need to mention "Meko" in your prompt (for example, _"save this to Meko,"_ _"search my Meko knowledge base"_) for the agent to call the tools. The agent won't proactively save conversations or capture memories unless you ask. To get automatic behavior, use the [Automated install](../quick-start/#connect-your-ai-client), or write your own skills to teach your agent when to call Meko tools.

## Report an issue

The fastest way to get help and report bugs is the Meko Discord. Join the community, post in the relevant channel, and the team or other community members will respond.

[Join the Meko Discord &rarr;](https://discord.gg/GWcSST7GqZ)

When reporting an issue, include:

- What you were trying to do
- What you expected to happen
- What actually happened
- Any error messages from your agent or the Meko portal
- The name of your datapack (not the API key) if the issue is datapack-specific

## Share feedback

While you're signed into the [Meko portal](https://cloud.mekodata.ai/), use the in-portal feedback control to send detailed feedback. The portal automatically attaches diagnostic information about your session, which makes it easier for the team to reproduce and triage what you're seeing.

The portal feedback path is the right one for:

- Feature requests
- Usability or UX issues
- Suggestions for documentation or examples
- Anything you want to share that isn't a blocking bug
