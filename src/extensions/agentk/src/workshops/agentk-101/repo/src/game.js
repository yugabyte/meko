// The rules of the game: plain functions on one global, Game, with no drawing and no timers.
// ui.js draws the state and calls step() on a timer; the tests require this file in Node.

var Game = (function () {
  var DIRS = {
    up: { x: 0, y: -1 },
    down: { x: 0, y: 1 },
    left: { x: -1, y: 0 },
    right: { x: 1, y: 0 },
  };

  var OPPOSITE = { up: 'down', down: 'up', left: 'right', right: 'left' };

  /** A new game on a board of cols × rows. opts.rand (0 ≤ n < 1) picks where food goes; Math.random by default. */
  function create(cols, rows, opts) {
    var state = {
      cols: cols,
      rows: rows,
      snake: [],
      dir: 'right',
      lastDir: 'right',
      food: null,
      score: 0,
      best: 0,
      over: false,
      rand: (opts && opts.rand) || Math.random,
    };
    newGame(state);
    return state;
  }

  /** Sets up a fresh board. */
  function newGame(state) {
    var y = Math.floor(state.rows / 2);
    var x = Math.floor(state.cols / 2);
    state.snake = [{ x: x, y: y }, { x: x - 1, y: y }, { x: x - 2, y: y }];
    state.dir = 'right';
    state.lastDir = 'right';
    state.over = false;
    placeFood(state);
    return state;
  }

  /** Points the snake a new way for its next step. Turning back on itself is ignored. */
  function turn(state, dir) {
    if (!DIRS[dir] || state.over) return state;
    // Compared with the way the snake last moved, not the last key: two quick keys within one
    // step could otherwise turn it back into its own neck.
    if (dir === OPPOSITE[state.lastDir]) return state;
    state.dir = dir;
    return state;
  }

  /** Moves the snake one cell: it eats, grows and scores on food, and the game ends on a wall or itself. */
  function step(state) {
    if (state.over) return state;
    var d = DIRS[state.dir];
    var head = state.snake[0];
    var next = { x: head.x + d.x, y: head.y + d.y };
    state.lastDir = state.dir;
    if (next.x < 0 || next.y < 0 || next.x >= state.cols || next.y >= state.rows) {
      state.over = true;
      return state;
    }
    var eats = state.food && next.x === state.food.x && next.y === state.food.y;
    // The tail moves out of the way first, unless the snake grows this step.
    var bodyEnd = eats ? state.snake.length : state.snake.length - 1;
    for (var i = 0; i < bodyEnd; i++) {
      if (state.snake[i].x === next.x && state.snake[i].y === next.y) {
        state.over = true;
        return state;
      }
    }
    state.snake.unshift(next);
    if (eats) {
      state.score += 1;
      if (state.score > state.best) state.best = state.score;
      placeFood(state);
    } else {
      state.snake.pop();
    }
    return state;
  }

  /** Puts the food on a free cell, picked with state.rand; none when the board is full. */
  function placeFood(state) {
    var free = state.cols * state.rows - state.snake.length;
    if (free <= 0) {
      state.food = null;
      return state;
    }
    var n = Math.floor(state.rand() * free);
    for (var y = 0; y < state.rows; y++) {
      for (var x = 0; x < state.cols; x++) {
        if (occupied(state, x, y)) continue;
        if (n === 0) {
          state.food = { x: x, y: y };
          return state;
        }
        n--;
      }
    }
    return state;
  }

  function occupied(state, x, y) {
    for (var i = 0; i < state.snake.length; i++) {
      if (state.snake[i].x === x && state.snake[i].y === y) return true;
    }
    return false;
  }

  return { create: create, newGame: newGame, turn: turn, step: step, placeFood: placeFood };
})();

if (typeof module === 'object') module.exports = Game;
