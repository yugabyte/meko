# Snake

A small Snake game that runs in a web page. No build and no dependencies.

## Play

Open `index.html` in a browser. Turn with the arrow keys or WASD. Eat the food to grow and score,
and don't hit the walls or yourself. **New game** starts again.

## Test

The rules in `src/game.js` have tests. With Node 18 or later:

```sh
node --test
```

## Ideas

- A high-score table: the top five scores, with the date of each.
- Difficulty levels: a faster snake, or walls inside the board.
