---
title: Work with artifacts
linkTitle: Use artifacts
description: Store and retrieve agent-generated files in a datapack over MCP
weight: 4450
---
An artifact is a file an agent makes, such as a report, a CSV, a PDF, or code output.
Meko stores artifacts in a datapack so you can get them back later, or let another agent
in the same datapack use them.

Two MCP tools do the work:

| Tool | What it does |
| :--- | :--- |
| `artifact_put` | Uploads a file and returns a hash you use to get the file back |
| `artifact_get` | Gets a file back, using its hash |

When you upload a file, Meko gives it a hash: a long ID built from the file's contents.
The hash is how you get the file back. If you upload the same file twice, you get the
same hash, and Meko doesn't store a second copy.

## Store a file

Ask your agent to save a file it just made. For example:

```text
Save this report to my datapack.
```

The agent calls `artifact_put` and gets back a hash. Keep the hash, because it's how you
get the file back.

You don't have to track hashes yourself. Ask the agent to save the hash as a memory with
a name you'll remember:

```text
Save this report to my datapack, and remember its hash as "Q3 sales report".
```

## Retrieve a file

To get a file back, give the agent its hash:

```text
Get the artifact with hash <hash> from my datapack.
```

If you saved the hash as a memory, ask for it by name instead:

```text
Find the hash for the "Q3 sales report" in my memories, then get that artifact.
```

The agent looks up the hash and calls `artifact_get`. Each time you retrieve a file,
Meko resets its expiry clock (see [When artifacts expire](#when-artifacts-expire)).

## Share a file with another agent

Any agent in the same datapack can get the file, as long as it has the hash. It doesn't
matter which agent uploaded it.

To share a file, do one of the following:

- Give the other agent the hash directly.
- Save the hash as a memory. Memory is shared across your agents in a datapack, so
  another agent can look it up in a later session.

## Browse artifacts in the portal

To see every artifact in a datapack, open the datapack in the Meko portal and go to
**Artifacts**. You can search by filename and download any file. Agents upload artifacts
over MCP; the portal is where you view and download them.

## Where files are stored

Meko chooses where to store a file based on its size. You get the file back the same way
either way.

| Size | Where it's stored | What `artifact_get` returns |
| :--- | :--- | :--- |
| Less than 1 MB | In the database | The file's contents |
| 1 MB or more | In the datapack's object storage | A path to the file on your computer |

For a large file, read it from the path that `artifact_get` returns instead of decoding
it from the response.

## When artifacts expire

Meko never deletes an artifact outright. An expired artifact is hidden, but you can bring
it back.

| Plan | When files expire | Storage (shared with knowledge base) |
| :--- | :--- | :--- |
| Free | 30 days after you last use the file | 100 MB total across all datapacks; 5 MB per file |
| Standard | Never | 5 GB total across all datapacks; 50 MB per file |
| Pro | Never | Custom |

Artifact bytes count toward the same storage pool as knowledge-base files — uploading an
artifact reduces the room left for KB uploads, and the other way around. See
[Work with knowledge bases](../working-with-knowledge-bases/) and
[Meko pricing](https://mekodata.ai/pricing/) for plan details.

Each time you retrieve a file, the clock resets, so a file you keep using on Free won't
expire. If you try to get an expired file, you get an error. To bring it back, upload
the same file again.

## Next steps

- [Meko MCP Server reference](../../reference/mcp-server/#artifact-tools) — artifact tools
  and their inputs
- [Work with memory](../working-with-memory/) — save an artifact's hash so you can find
  it later
- [Work with knowledge bases](../working-with-knowledge-bases/)
