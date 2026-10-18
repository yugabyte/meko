// Package sqlitevec compiles sqlite-vec into the binary and registers it with SQLite, so every
// connection the store opens has the vec0 virtual table and the vec_* functions.
//
// sqlite-vec is built with SQLITE_CORE against the SQLite that mattn/go-sqlite3 compiles into the
// same binary, using the headers vendored in third_party/sqlite-vec.
package sqlitevec

// #cgo CFLAGS: -DSQLITE_CORE -O2 -I${SRCDIR}/../../third_party/sqlite-vec
// #cgo linux LDFLAGS: -lm
// #include "sqlite-vec.h"
// #include "sqlite3.h"
// static const char *header_version(void) { return SQLITE_VERSION; }
import "C"

import "sync"

var once sync.Once

// Register makes every SQLite connection opened after it load sqlite-vec. It's safe to call more
// than once.
func Register() {
	once.Do(func() {
		C.sqlite3_auto_extension((*[0]byte)(C.sqlite3_vec_init))
	})
}

// HeaderVersion is the SQLite version of the vendored headers sqlite-vec is compiled against.
func HeaderVersion() string {
	return C.GoString(C.header_version())
}
