---
title: Docs style guide
linkTitle: Docs style guide
weight: 10000
body_class: full-width
type: docs
---

## Admonition boxes

### Tip

> [!TIP]
>
> Did you know that...

```md
> [!TIP]
>
> Did you know that...
```

### Note

> [!NOTE] Version note
>
> This feature is only available in version 2.0 and later.

```md
> [!NOTE] Version note
>
> This feature is only available in version 2.0 and later.
```

### Warning

> [!WARNING] warning: Blank line required!
>
> This site uses the [Prettier] formatter, and it requires an empty line
> separating the alert tag/title from the alert body.

```md
> [!WARNING] warning: Blank line required!
>
> This site uses the [Prettier] formatter, and it requires an empty line
> separating the alert tag/title from the alert body.
```

### Danger

> [!DANGER] Danger Will Robinson!

```md
> [!DANGER] Danger Will Robinson!
```

### NB

> [!NB] **Skip this section** if the package is already installed.

```md
> [!NB] **Skip this section** if the package is already installed.
```

## Tabs

{{< tabpane text=true right=false >}}
{{% tab header="**Languages**:" disabled=true /%}}
{{% tab header="English" lang="en" %}}
Meko is an agent-native data layer for multi-agent systems that work and learn together.  It supports:

- **Collective memory**: learning that compounds across the entire system, as opposed to just per-agent memory
- **Shared knowledge**: building knowledge over time from conversations, real-time data sources and slower-changing knowledge bases
- **Auditability of the learning process**: connect execution traces to how agents process data and knowledge, learn from it and share that learning

Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is serverless, multi-tenant (multi-agentic) and optimized for tiering to object stores. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search.

{{% /tab %}}
{{% tab header="German" lang="de" %}}
Meko is an agent-native data layer for multi-agent systems that work and learn together.  It supports:
{{% /tab %}}
{{% tab header="Chines" lang="ch" %}}
Meko is an agent-native data layer for multi-agent systems that work and learn together.  It supports:
{{% /tab %}}
{{< /tabpane >}}

```md
{{</* tabpane text=true right=false */>}}
{{%/*tab header="**Languages**:" disabled=true */%}}
{{%/*tab header="English" lang="en" */%}}
Meko is an agent-native data layer for multi-agent systems that work and learn together.  It supports:

- **Collective memory**: learning that compounds across the entire system, as opposed to just per-agent memory
- **Shared knowledge**: building knowledge over time from conversations, real-time data sources and slower-changing knowledge bases
- **Auditability of the learning process**: connect execution traces to how agents process data and knowledge, learn from it and share that learning

Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is serverless, multi-tenant (multi-agentic) and optimized for tiering to object stores. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search.

{{%/*/tab */%}}
{{%/*tab header="German" lang="de" */%}}
Meko is an agent-native data layer for multi-agent systems that work and learn together.  It supports:
{{%/*/tab */%}}
{{%/*tab header="Chines" lang="ch" */%}}
Meko is an agent-native data layer for multi-agent systems that work and learn together.  It supports:
{{%/*/tab */%}}
{{</* /tabpane */>}}
```

## Cards

### Cards without Variants

#### Cards with all available options

{{< card-group >}}
  {{< card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
    desc="Sign up, connect, and see Meko in action in under 60 seconds."
    cta="Get started &rarr;"
  >}}
  {{< card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
  >}}
  {{< card
    href="examples/"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
    image="images/database.svg"
  >}}
  {{< card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
    desc="Sign up, connect, and see Meko in action in under 5 minutes."
    cta="Get started &rarr;"
    image="images/database.svg"
  >}}
  {{< card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
    image="images/database.svg"
  >}}
  {{< card
    href="examples/"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
    image="images/database.svg"
  >}}
{{< /card-group >}}

```md
{{</* card-group */>}}
  {{</* card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
    desc="Sign up, connect, and see Meko in action in under 5 minutes."
    cta="Get started &rarr;"
  */>}}
  {{</* card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
  */>}}
  {{</* card
    href="examples/"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
    image="images/database.svg"
  */>}}
  {{</* card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
    desc="Sign up, connect, and see Meko in action in under 5 minutes."
    cta="Get started &rarr;"
    image="images/database.svg"
  */>}}
  {{</* card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
    image="images/database.svg"
  */>}}
  {{</* card
    href="examples/"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    cta="View examples &rarr;"
    image="images/database.svg"
  */>}}
{{</* /card-group */>}}
```

#### Cards without Description and Image

{{< card-group variant="third">}}
  {{< card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
    cta="Get started &rarr;"
  >}}
  {{< card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
    cta="View examples &rarr;"
  >}}
  {{< card
    href="examples/"
    title="Examples"
    cta="View examples &rarr;"
  >}}
{{< /card-group >}}

```md
{{</* card-group variant="third" */>}}
  {{</* card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
    cta="Get started &rarr;"
  */>}}
  {{</* card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
    cta="View examples &rarr;"
  */>}}
  {{</* card
    href="examples/"
    title="Examples"
    cta="View examples &rarr;"
  */>}}
{{</* /card-group */>}}
```

#### Cards without CTA

{{< card-group >}}
  {{< card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start"
    desc="Sign up, connect, and see Meko in action in under 5 minutes."
    image="images/database.svg"
  >}}
  {{< card
    href="examples/"
    icon="fa-brands fa-python"
    title="Example"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    image="images/database.svg"
  >}}
  {{< card
    href="examples/"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    image="images/database.svg"
  >}}
{{< /card-group >}}

```md
{{</* card-group */>}}
  {{</* card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start"
    desc="Sign up, connect, and see Meko in action in under 5 minutes."
    image="images/database.svg"
  */>}}
  {{</* card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    image="images/database.svg"
  */>}}
  {{</* card
    href="examples/"
    title="Examples"
    desc="Try patterns for coding preferences, knowledge bases, and resumable agents."
    image="images/database.svg"
  */>}}
{{</* /card-group */>}}
```

#### Cards without Description, CTA & Image

{{< card-group variant="third" >}}
  {{< card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
  >}}
  {{< card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
  >}}
  {{< card
    href="examples/"
    title="Examples"
  >}}
{{< /card-group >}}

```md
{{</* card-group variant="third" */>}}
  {{</* card
    href="quick-start/"
    logo="logos/integrations/claude.svg"
    title="Quick Start (With Logo)"
  */>}}
  {{</* card
    href="examples/"
    icon="fa-brands fa-python"
    title="Examples (With FA Icon)"
  */>}}
  {{</* card
    href="examples/"
    title="Examples"
  */>}}
{{</* /card-group */>}}
```

### Cards with 1 Full Box per Row

{{< card-group variant="full" >}}
  {{< card
    href="quick-start/"
    title="Quick Start"
    desc="Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search."
    cta="Get started &rarr;"
    image="images/database.svg"
  >}}
{{< /card-group >}}

```md
{{</* card-group variant="full" */>}}
  {{</* card
    href="quick-start/"
    title="Quick Start"
    desc="Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search."
    cta="Get started &rarr;"
    image="images/database.svg"
  */>}}
{{</* /card-group */>}}
```

### Cards with 1 Box (Third Quarter) per Row

{{< card-group variant="three-quarter" >}}
  {{< card
    href="quick-start/"
    title="Quick Start"
    desc="Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search."
    cta="Get started &rarr;"
    image="images/database.svg"
  >}}
{{< /card-group >}}

```md
{{</* card-group variant="three-quarter" */>}}
  {{</* card
    href="quick-start/"
    title="Quick Start"
    desc="Meko is fully open-source and integrates easily with any agentic framework through MCP servers. It is built on top of a unified distributed PostgreSQL data layer that supports vector, SQL, NoSQL, graph, and search."
    cta="Get started &rarr;"
    image="images/database.svg"
  */>}}
{{</* /card-group */>}}
```

> [!NOTE] Layout Variants
>
> You can use `variant="half"` to display two boxes per row, `variant="full"` to display a single box per row, or omit `variant` to use the default 3-column layout.
