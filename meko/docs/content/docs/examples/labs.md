---
title: Labs
linkTitle: Labs
description: Try Meko features using a live demo application in the portal
weight: 2005
type: docs
---

Use Labs to explore core Meko features with a live demo application connected to a real datapack. Each lab walks a stage of the agentic development lifecycle: you build the context, an agent uses it, and you see exactly what it changed. Every lab ends with a path back to your own repo. Nothing to install.

Labs sit in that lifecycle — Spec, Build, Review, Fix & merge — with Review live today.

To run a lab:

1. [Sign up](https://cloud.mekodata.ai/).
1. Log in.
1. In the portal sidebar, open **Labs**, choose a scenario, and click **Launch lab**.
1. Follow the on-screen instructions.

## Labs

The following lab is available (with more in development).

### Review a PR with AgentK

Learn how an agent reviews a pull request against your org's coding standards, live. You start with a sample project that has no standards yet, build that context in a [datapack](../../architecture/core-concepts/datapacks/), then watch [AgentK](https://github.com/yugabyte/canned-agents/tree/main/adlc-review-agent) use it.

The lab takes about five minutes across four chapters: give the agent context, share it with AgentK, read the review, and close the loop on a pull request of your own. AgentK runs on AWS Bedrock AgentCore.

## Limitations

- Creating a datapack during a lab counts against your plan's datapack quota. Free accounts can create up to **10 datapacks**. See [Meko pricing](https://mekodata.ai/pricing/).
- Free accounts use the shared, predeployed AgentK. Deploying a dedicated instance requires a paid plan.
- Pointing AgentK at your own pull request is optional. Your GitHub personal access token stays in the browser; Meko never receives it. The PR title and full diff are then sent to Meko and the review agent — on the Free tier, the shared predeployed instance.
