# Vendored sqlite-vec

`sqlite-vec.c` and `sqlite-vec.h` are the sqlite-vec release amalgamation named in `VERSION`,
unchanged. They're dual-licensed MIT or Apache-2.0 (`LICENSE-MIT`, `LICENSE-APACHE`).
`internal/sqlitevec` compiles them into the binary with cgo.

`sqlite3-binding.h` and `sqlite3ext.h` are SQLite's headers (public domain) as shipped in the
mattn/go-sqlite3 version in `go.mod`, and `sqlite3.h` points at them. cgo can't take headers
from the module cache, so they live here. They must match the SQLite that mattn compiles:
`TestHeadersMatchCompiledSQLite` fails when they don't.

To upgrade: replace the files, update `VERSION`, and run `make test`. After a mattn upgrade, copy
its two headers here again.
