<p align="center"><img src="assets/Presenter-nobg.png" width="200" alt="Presenter"></p>

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
build decks; playing and editing them work without Claude. It has no releases yet, so run it from
source:

```sh
cd extensions/presenter
npm ci && npm run build && npm link     # puts `presenter` on PATH
meko install_extension presenter        # registers it with this Meko (it asks Meko for its key)
meko start                              # starts Meko and its extensions
meko ui /ext/presenter/                 # opens Presenter
```

Meko serves it at `/ext/presenter/` on its web listener, behind Meko's login, and its sidebar links
to it. `presenter status` says whether it runs.

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
