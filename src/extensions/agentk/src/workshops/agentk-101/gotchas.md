# Gotchas: agentk-demo

Traps earlier work on this repo hit, mined from its history. Each one is a fact the code alone
doesn't show.

## MG-1: The cabinet runs step() at 60 Hz on a slow board

**Where:** `src/game.js` (`step`)

The arcade cabinet build calls `Game.step()` 60 times a second on a low-power board, much faster
than the browser's 120 ms tick. A `step()` that copies the whole snake or rescans the board on
every call drops frames on the cabinet once the snake is long. `step()` must move the snake in
place (add the head, drop the tail) and only scan the board when food is eaten.

**From:** the cabinet's frame-drop report after the "immutable state" refactor, which was reverted.

## MG-2: The daily challenge carries the score through newGame()

**Where:** `src/game.js` (`newGame`), `src/ui.js` (the New game button)

The arcade cabinet's daily challenge, built from another repo that loads this repo's
`src/game.js`, calls `Game.newGame()` between its three rounds and adds the rounds' scores up.
Resetting the score in `newGame()` zeroes the daily challenge's running total after every round,
so every player's challenge score is only their last round's. `newGame()` must keep
`state.score`: reset the score where a player starts a new run (the New game button's handler, or
a function such as `Game.startRun()` that it calls), never in `newGame()`.

**From:** the daily challenge leaderboard showing only last-round scores, after a score reset was
added to `newGame()` and then reverted.

## MG-3: The cabinet's joystick sends two turns within one step

**Where:** `src/game.js` (`turn`)

The cabinet's joystick often reports two directions within one 16 ms step, such as up then left
while the snake moves right. When `turn()` checks a new direction against the last key instead of
the direction the snake last moved, the second turn points the snake back into its own neck and
the game ends. `turn()` must compare against the direction of the last step (`lastDir`).

**From:** players' reports of games ending on a quick turn, on the cabinet only.
