# Security

This policy covers everything in this repo: Meko (`backend/`), AgentK (`extensions/agentk`) and
Presenter (`extensions/presenter`), and the releases built from them.

## Reporting a vulnerability

Report it privately to **security@yugabyte.com**. Don't open a GitHub issue, PR or discussion
for it: those are public, and so is anything attached to them.

Include what you can of:

- the product and version (`meko version`, or the release tag) and how you installed it;
- what the vulnerability lets an attacker do, and how to reproduce it;
- any proof of concept, logs or screenshots, with secrets and personal data removed.

We'll acknowledge the report and keep you posted while we fix it. Please give us a reasonable
time to ship a fix before you publish anything about it.

## Scope

In scope: the code in this repo and the release artifacts it produces. Reports about Meko Cloud
(mekodata.ai) go to the same address. A vulnerability in a third-party dependency belongs with
its own maintainers; tell us too, so we can update.

## Fixes

Security fixes ship as ordinary releases on each product's release train, and the release notes
say what was fixed once the fix is out.
