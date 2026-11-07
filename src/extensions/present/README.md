<p align="center"><img src="assets/Present-nobg.png" width="200" alt="Present"></p>

<p align="center">
  <a href="https://mekodata.ai">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="../../../assets/badges/meko-context-dark.svg">
      <img src="../../../assets/badges/meko-context.svg" width="112" height="30" alt="Meko context">
    </picture>
  </a>
</p>

# Present

Presentations that branch with the room, built from what your team keeps in Meko.

Pick the datapacks to draw from and write a short brief. Claude drafts a **story map**: scenes,
forks where the room picks a path, returns to a hub, and detours you can jump to. Walk through it
on a canvas, reshape it, then let Claude build the slides scene by scene. Edit anything by hand,
or ask in plain language ("add a fork after xCluster on HA vs. cost", "shorter, for a CFO");
Claude's changes come back as a change set you review.

During the talk, the **console** shows the slide, what's next, your notes and the slide's sources.
Pick a fork with a number key, switch between the overview and the deep dive, jump to a detour and
come back, or blank the screen. The **audience window** follows on the projector.

Every slide carries the Meko memories, knowledge files or web pages it rests on, and a slide
without any is flagged. Decks are saved in their own Meko datapack (`present/deck/<name>`), in
pieces, so a deck can be bigger than Meko's 5 MiB artifact limit: up to 100 MiB per file and 1 GiB
a deck.

## Run it

Present needs [Meko](../../../README.md) on this computer and Claude Code (`claude`, signed in) to
build decks; playing and editing them work without Claude. Meko installs it, from Present's
newest release (or Install on the web UI's Extensions page):

```sh
meko extension install present    # installs the newest release, pointed at this Meko
meko start                        # starts Meko and its extensions
meko ui                           # opens Meko's web UI: Present is in its sidebar
```

Meko serves it at `/ext/present/` on its port, with Meko's access check, and its sidebar links
to it. Decks are saved where Meko says: in Meko Cloud when Meko links it (Settings › Meko Cloud, or
`meko cloud-link`), else in the Meko on this computer. Either way Present talks to the Meko on
this computer, which passes its calls on to Meko Cloud with the link's key. While it links Meko
Cloud, the decks in its own datapacks (made before the link) keep working beside Meko Cloud's.
Present follows a change of the link at once; a draft made under the other Meko stays listed, and
opens again once Meko links it again. Its commands go through Meko: `meko present status` says whether it runs (`meko present help`
lists the rest). Meko
updates it on its own when a new release comes out (unless its **Update automatically** switch on
the Extensions page is off), waiting while a talk is live; `meko extension update present` (or
Update on the Extensions page) updates it now. A release that fails to start three times is rolled
back to the one before, and never offered again.

To run a build from source instead, put it on PATH before installing it: Meko uses it as it is,
and never updates it. Its `me_present` on PATH is only for Meko to find; run it as `meko present
<command>` like a release.

```sh
cd src/extensions/present
npm ci && npm run build && npm link   # puts `me_present` on PATH, for Meko to find
meko extension install present        # registers it with this Meko (it asks Meko for its key)
```

## Datapacks

| Datapack | Holds |
|---|---|
| Any datapack | Content: whatever memories and knowledge files it has. Present only reads it. |
| `present/template/<style>` | A style (later; every deck uses the built-in template for now). |
| `present/anim/<area>` | Canned animations (later; slides name them for now). |
| `present/deck/<name>` | A deck: `deck.json`, its scenes, files and versions. |

## Develop

[AGENTS.md](AGENTS.md) has the map, the commands and the rules; [docs/design.md](docs/design.md)
the design.
