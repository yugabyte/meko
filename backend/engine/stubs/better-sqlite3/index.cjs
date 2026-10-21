// mem0ai imports better-sqlite3 for its own SQLite stores. Meko's engine stores through the Go
// server instead, so it never opens one; this stub keeps the native module out of the install.
module.exports = function Database() {
  throw new Error("better-sqlite3 isn't part of Meko's engine");
};
