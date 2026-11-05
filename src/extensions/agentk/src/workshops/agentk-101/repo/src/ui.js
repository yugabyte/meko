// Drawing and input: the canvas, the keys and the New game button. The rules are in game.js.

(function () {
  var COLS = 20;
  var ROWS = 20;
  var CELL = 20;
  var TICK_MS = 120;

  var KEYS = {
    ArrowUp: 'up', ArrowDown: 'down', ArrowLeft: 'left', ArrowRight: 'right',
    w: 'up', s: 'down', a: 'left', d: 'right',
  };

  var canvas = document.getElementById('board');
  var ctx = canvas.getContext('2d');
  var scoreEl = document.getElementById('score');
  var bestEl = document.getElementById('best');
  var statusEl = document.getElementById('status');
  var state = Game.create(COLS, ROWS);

  canvas.width = COLS * CELL;
  canvas.height = ROWS * CELL;

  function draw() {
    ctx.fillStyle = '#10261c';
    ctx.fillRect(0, 0, canvas.width, canvas.height);
    if (state.food) {
      ctx.fillStyle = '#f2b84b';
      ctx.beginPath();
      ctx.arc(state.food.x * CELL + CELL / 2, state.food.y * CELL + CELL / 2, CELL / 2 - 3, 0, Math.PI * 2);
      ctx.fill();
    }
    for (var i = 0; i < state.snake.length; i++) {
      ctx.fillStyle = i === 0 ? '#9be58a' : '#5fbf6a';
      var c = state.snake[i];
      ctx.fillRect(c.x * CELL + 1, c.y * CELL + 1, CELL - 2, CELL - 2);
    }
    scoreEl.textContent = String(state.score);
    bestEl.textContent = String(state.best);
    statusEl.textContent = state.over ? 'Game over. Press New game to play again.' : '';
  }

  function tick() {
    Game.step(state);
    draw();
  }

  document.addEventListener('keydown', function (e) {
    var dir = KEYS[e.key] || KEYS[e.key.toLowerCase()];
    if (!dir) return;
    e.preventDefault();
    Game.turn(state, dir);
  });

  document.getElementById('new-game').addEventListener('click', function () {
    Game.newGame(state);
    draw();
  });

  draw();
  setInterval(tick, TICK_MS);
})();
