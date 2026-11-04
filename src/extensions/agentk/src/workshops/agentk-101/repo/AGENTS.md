# AGENTS.md

A Snake game that runs in a web page, with no build and no dependencies.

- `index.html` loads `style.css`, `src/game.js` and `src/ui.js`.
- `src/game.js`: the rules, as plain functions on the `Game` global. No drawing, timers or DOM.
- `src/ui.js`: the canvas, the keys and the New game button.
- `test/game.test.js`: tests of the rules.

## Run and test

- Play: open `index.html` in a browser.
- Test: `node --test` (Node 18 or later).

## Rules

- Keep the scripts plain: no ES modules (`import`, `export`, `type="module"`) and no `fetch`.
  `index.html` is opened from disk, and browsers block both on a page opened from a file.
- Rules go in `game.js`, drawing and input in `ui.js`. `game.js` ends with
  `module.exports = Game` so the tests can require it.
- New rules get a test.
