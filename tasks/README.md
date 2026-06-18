# tasks/

> **This directory is retired.** Cross-session work is now tracked as
> **GitHub issues** on [`yugabyte/yb-light`](https://github.com/yugabyte/yb-light/issues),
> not as Markdown files. The old `tasks/*.md` files were migrated to
> issues and deleted; their full history is in git.

See **[`../docs/issue-tracking.md`](../docs/issue-tracking.md)** — the
source of truth for the issue conventions.

Quick reference:

```sh
gh issue list --label storage --state all        # everything storage
gh issue list --milestone POC --state open         # open POC work
gh issue create --title "..." --body "..." \
  --label storage --milestone POC                   # label + milestone required
```

Or browse <https://github.com/yugabyte/yb-light/issues>.
