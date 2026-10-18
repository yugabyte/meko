# Vendored sqlite-vec

`sqlite-vec.c` and `sqlite-vec.h` are the sqlite-vec release amalgamation named in `VERSION`,
unchanged. They're dual-licensed MIT or Apache-2.0 (`LICENSE-MIT`, `LICENSE-APACHE`).
`internal/sqlitevec` compiles them into the binary with cgo.

`sqlite3-binding.h` is SQLite's header (public domain) as shipped in the mattn/go-sqlite3 version
in `go.mod`, and `sqlite3.h` points at it. cgo can't take headers from the module cache, so it
lives here. It must match the SQLite that mattn compiles: `TestHeadersMatchCompiledSQLite` fails
when it doesn't. sqlite-vec is built with `SQLITE_CORE` (as part of the program, not a loadable
extension), so it needs no `sqlite3ext.h`.

To upgrade sqlite-vec: replace `sqlite-vec.c` and `sqlite-vec.h`, update `VERSION` (the test reads
the expected version from its first line), and run `make test`. After a mattn upgrade, copy its
`sqlite3-binding.h` here again.
