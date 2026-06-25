---
title: API keys
linkTitle: API keys
description: Generate, name, and revoke API keys for connecting MCP clients to your datapacks
weight: 50
type: docs
---

Meko uses long-lived API keys (personal access tokens) to authenticate MCP clients. You manage them in the portal under **Settings > API Keys**.

## Generate an API key

**From Settings**

1. In the Meko portal, open **Settings**.
2. In the **API Keys** section, click **Generate API key**.
3. Enter a **Name** that identifies the client (for example, `laptop/claude-desktop`, `laptop/cursor`). This is a label only; pick something that tells you where the key is used.
4. Click **Generate API key**.
5. **Copy** or **Download .txt** before closing the dialog. The full key is shown exactly once.

**From the datapack Connect dialog**

On any datapack, click **Connect**. If you have no active API key, the dialog prompts you to generate one (same flow as Settings). If you already have keys, they're listed by name and prefix so you know which saved key to paste into install commands.

## Key format

All keys start with `mko_tkn_` followed by a random string. Only the prefix is shown after creation — the full key cannot be recovered. If you lose it, revoke the old key and generate a new one.

## Limits

- Up to **10 active keys** per account.
- Each key expires after **one year** (365 days) from creation.
- Keys are scoped to your account, not to a single datapack — any key can access all your datapacks.

## Revoke a key

1. Go to **Settings > API Keys**.
2. Find the key and click **Revoke**.
3. Confirm in the dialog.

Revocation is **immediate and permanent** — any MCP client using that key stops working as soon as you confirm. The key remains in your list with a **REVOKED** status for your records.

## Best practices

- **One key per client.** Name keys after the machine and client they're used with (for example, `laptop/cursor`, `workstation/claude-code`). This makes it easy to revoke a single client without disrupting others.
- **Rotate before expiry.** Generate a new key, update your client configuration, then revoke the old one.
- **Download the .txt** as a backup when generating. You can't retrieve the full key later.
