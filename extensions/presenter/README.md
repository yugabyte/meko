<p align="center"><img src="assets/Presenter-nobg.png" width="200" alt="Presenter"></p>

<p align="center">
  <a href="https://mekodata.ai">
    <picture>
      <source media="(prefers-color-scheme: dark)" srcset="../../backend/assets/badges/meko-context-dark.svg">
      <img src="../../backend/assets/badges/meko-context.svg" width="112" height="30" alt="Meko context">
    </picture>
  </a>
</p>

# Presenter

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
without any is flagged. Decks are saved in their own Meko datapack (`presenter/deck/<name>`), in
pieces, so a deck can be bigger than Meko's 5 MiB artifact limit: up to 100 MiB per file and 1 GiB
a deck.

## Run it

Presenter needs [Meko](../../README.md) on this computer and Claude Code (`claude`, signed in) to
build decks; playing and editing them work without Claude. Meko installs it, from Presenter's
newest release (or Install on the web UI's Extensions page):

```sh
meko extension install presenter        # installs the newest release, pointed at this Meko
meko start                              # starts Meko and its extensions
meko ui /ext/presenter/                 # opens Presenter
```

Meko serves it at `/ext/presenter/` on its web listener, behind Meko's login, and its sidebar links
to it. Decks are saved where Meko says: in Meko Cloud when Meko links it (Settings › Meko Cloud, or
`meko cloud_link`), else in the Meko on this computer. Either way Presenter talks to the Meko on
this computer, which passes its calls on to Meko Cloud with the link's key. While it links Meko
Cloud, the decks in its own datapacks (made before the link) keep working beside Meko Cloud's.
Presenter follows a change of the link at once; a draft made under the other Meko stays listed, and
opens again once Meko links it again. Presenter's command is `meko_presenter`: `meko_presenter status` says whether it runs. Meko
updates it on its own when a new release comes out (unless its **Update automatically** switch on
the Extensions page is off), waiting while a talk is live; `meko extension update presenter` (or
Update on the Extensions page) updates it now. A release that fails to start three times is rolled
back to the one before, and never offered again.

To run a build from source instead, put it on PATH before installing it: Meko uses it as it is,
and never updates it.

```sh
cd extensions/presenter
npm ci && npm run build && npm link     # puts `meko_presenter` on PATH
meko extension install presenter        # registers it with this Meko (it asks Meko for its key)
```

## Datapacks

| Datapack | Holds |
|---|---|
| Any datapack | Content: whatever memories and knowledge files it has. Presenter only reads it. |
| `presenter/template/<style>` | A style (later; every deck uses the built-in template for now). |
| `presenter/anim/<area>` | Canned animations (later; slides name them for now). |
| `presenter/deck/<name>` | A deck: `deck.json`, its scenes, files and versions. |

## Develop

[AGENTS.md](AGENTS.md) has the map, the commands and the rules; [docs/design.md](docs/design.md)
the design.
