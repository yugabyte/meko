const test = require('node:test');
const assert = require('node:assert');
const Game = require('../src/game.js');

// Food at the first free cell, so where it lands is known.
const first = () => 0;

test('the snake moves one cell a step', () => {
  const s = Game.create(10, 10, { rand: first });
  const head = s.snake[0];
  Game.step(s);
  assert.deepStrictEqual(s.snake[0], { x: head.x + 1, y: head.y });
  assert.strictEqual(s.snake.length, 3);
});

test('turning back on itself is ignored', () => {
  const s = Game.create(10, 10, { rand: first });
  Game.turn(s, 'left');
  assert.strictEqual(s.dir, 'right');
});

test('two quick keys within one step cannot turn the snake into its neck', () => {
  const s = Game.create(10, 10, { rand: first });
  Game.turn(s, 'up');
  Game.turn(s, 'left');
  assert.strictEqual(s.dir, 'up');
});

test('eating food grows the snake and scores', () => {
  const s = Game.create(10, 10, { rand: first });
  const head = s.snake[0];
  s.food = { x: head.x + 1, y: head.y };
  Game.step(s);
  assert.strictEqual(s.score, 1);
  assert.strictEqual(s.snake.length, 4);
  assert.notDeepStrictEqual(s.food, { x: head.x + 1, y: head.y });
});

test('hitting a wall ends the game', () => {
  const s = Game.create(10, 10, { rand: first });
  for (let i = 0; i < 10; i++) Game.step(s);
  assert.strictEqual(s.over, true);
});

test('the best score is kept across games', () => {
  const s = Game.create(10, 10, { rand: first });
  const head = s.snake[0];
  s.food = { x: head.x + 1, y: head.y };
  Game.step(s);
  Game.newGame(s);
  assert.strictEqual(s.best, 1);
  assert.strictEqual(s.over, false);
});
