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

import (
	"fmt"
	"sync"

	// sqlite-vec and Register call SQLite's C API, which only mattn/go-sqlite3 compiles into
	// the binary: importing it here keeps every user of this package linkable.
	_ "github.com/mattn/go-sqlite3"
)

var once sync.Once

// Register makes every SQLite connection opened after it load sqlite-vec. It's safe to call more
// than once.
func Register() {
	once.Do(func() {
		if rc := C.sqlite3_auto_extension((*[0]byte)(C.sqlite3_vec_init)); rc != C.SQLITE_OK {
			// Without it no connection has vec0; there's no way to go on, and nowhere to return to.
			panic(fmt.Sprintf("sqlitevec: sqlite3_auto_extension failed with SQLite error %d", int(rc)))
		}
	})
}

// HeaderVersion is the SQLite version of the vendored headers sqlite-vec is compiled against.
func HeaderVersion() string {
	return C.GoString(C.header_version())
}
