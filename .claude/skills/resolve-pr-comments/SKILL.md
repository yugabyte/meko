---
name: resolve-pr-comments
description: >-
  Close out review comments on a PR in this repo: resolve the threads whose
  findings have actually been dealt with, and react 👍/👎 on each automated
  finding to say whether the finding itself was *correct* — the vote is training
  signal for the reviewer, not a record of whether you fixed it. Use for "resolve
  the addressed comments", "address the review comments", "close out the review
  threads", "resolve the review", or after pushing fixes in response to a review.
---

# resolve-pr-comments

Two separate jobs, and conflating them is the main way this goes wrong:

- **Resolving** a thread says *"this is dealt with."* It is about your change.
- **👍/👎** says *"the reviewer was right / wrong to raise this."* It is about the
  **reviewer**, and it is the only feedback loop that improves the review logic.

They come apart constantly. A correct finding you consciously decline still gets
👍 and stays unresolved-or-replied. A wrong finding gets 👎 and gets resolved
without a code change. If you 👍 everything you fixed and 👎 everything you
didn't, the vote just re-encodes your diff and the signal is worthless.

## Step 1 — list the threads

```
gh api graphql -f query='{ repository(owner:"yugabyte",name:"meko"){ pullRequest(number:NNN){
    reviewThreads(first:100){ nodes{ id isResolved isOutdated path line
      comments(first:1){ nodes{ databaseId author{login __typename} body } } } } } } }' \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[]
        | select(.isResolved==false)
        | "\(.id)\t\(.comments.nodes[0].databaseId)\t\(.comments.nodes[0].author.__typename)\t\(.comments.nodes[0].author.login)\t\(.path):\(.line)"'
```

You need **both** ids, for different things:

- **`thread.id`** (GraphQL node id, `PRRT_…`) — to reply and to resolve.
- **`comments.nodes[0].databaseId`** (REST integer) — to react. Reactions have no
  GraphQL-node path from the thread, so this is the one place REST is required.

They are not interchangeable, and crossing them either errors or acts on an
unrelated comment.

**`isOutdated` is not "addressed".** It only means the diff moved underneath the
comment — which happens for unrelated edits, rebases, and force-pushes. Resolving
on `isOutdated` alone silently buries findings that were never looked at.

## Step 2 — verify each finding against the code

Read the claim and check it, in the tree, before deciding anything. Bot findings
here are specific and cite line numbers, which makes them cheap to verify and easy
to accept on plausibility alone — resist that. Follow the chain the finding
asserts and confirm each link.

Findings from these reviewers have been **right about non-obvious things**: one
caught that dropping a database in `StartAll` would break every `start` after the
first `endpoint create`, because pageservers are rediscovered from disk and claim
a generation against a row the drop had removed. That was correct, load-bearing,
and nothing in the diff looked wrong. They have also been **wrong**. Verifying is
what tells you which, and it is exactly what the 👍/👎 is asking you to report.

Land on one of four outcomes per finding:

| Outcome | Vote | Resolve? |
|---|---|---|
| Correct, and you fixed it | 👍 | yes — after the fix is pushed |
| Correct, but you are deliberately not fixing it | 👍 | reply with the reason, then resolve |
| Wrong — the claim does not hold | 👎 | reply with what it got wrong, then resolve |
| Unsure | no vote | leave open |

**No vote is a legitimate outcome.** A guessed vote is worse than silence,
because it is indistinguishable from a considered one.

## Step 3 — react

```
gh api -X POST repos/yugabyte/meko/pulls/comments/<databaseId>/reactions -f content='+1'
gh api -X POST repos/yugabyte/meko/pulls/comments/<databaseId>/reactions -f content='-1'
```

React on the **first comment of the thread** — that is the finding. Do not react
to your own replies.

**Post the reaction whether or not the buttons are already there.** Some reviewers
pre-seed both `+1` and `-1` on their own findings so a human can click one; others
post nothing. `POST` covers both: it creates the reaction when absent and is
idempotent when it already exists, returning the same reaction id. Never remove a
reviewer's own seeded pair — the seeds are the buttons, not votes.

Vote on **automated** findings only. A 👎 on a colleague's review comment reads as
a put-down, not as data; reply to them instead. Identify them by the author's
`__typename` being `Bot` (`.user.type` on the REST side), never by matching a
name — the reviewers change.

## Step 4 — reply where the outcome is not self-evident

A resolve with no explanation is unreadable later. Reply first when you are
declining a correct finding, or rejecting one as wrong — say *why*, with the same
specificity the finding used. If you fixed it, name the commit:

```
gh api graphql -f query='mutation($t:ID!,$b:String!){
  addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t, body:$b}){ comment{ url } } }' \
  -f t='<thread.id>' -f b='Fixed in <sha>. <what changed>'
```

## Step 5 — resolve

```
gh api graphql -f query='mutation($t:ID!){ resolveReviewThread(input:{threadId:$t}){ thread{ isResolved } } }' -f t='<thread.id>'
```

Only after the fix is **pushed**. Resolving against a local edit claims something
the PR does not yet show.

Resolved one by mistake? `unresolveReviewThread` takes the same input and puts it
back.

## Anti-patterns

- **Bulk-resolving every `isOutdated` thread.** The most tempting shortcut and the
  most damaging: it looks like diligence and buries findings nobody read.
- **Voting to match your diff.** 👍 = "you were right", not "I did it". Fixing a
  wrong finding to make it go away, then 👍-ing it, actively trains the reviewer
  to repeat the mistake.
- **Resolving your own unaddressed thread to unblock a merge.** If it is blocking
  and you disagree, say so in a reply and let a human decide.
- **Accepting a finding because it is well-written.** These reviewers cite exact
  lines and read as authoritative whether or not they are correct. Check the
  claim, not the confidence.
- **Reacting to the wrong comment id.** `thread.id` is GraphQL, `databaseId` is
  REST; crossing them errors, or worse, reacts to an unrelated comment.
- **👎 on a human's comment.** Reply instead.
