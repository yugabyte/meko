package main

import (
	"github.com/yugabyte/yb-amp/controller/internal/core"
	"github.com/yugabyte/yb-amp/controller/internal/task"
)

// Compile-time assertions that the concrete core backends satisfy the task
// framework's StateStore port. The binding is structural (the task package does
// not import core, to stay decoupled), so without these an interface/impl drift
// would only surface where the backend is assigned into task.Deps (main.go) —
// here it fails the build at the source of truth instead. *PgBackend is the
// production state store; *MemBackend is the in-memory one the API/worker tests
// drive.
var (
	_ task.StateStore = (*core.PgBackend)(nil)
	_ task.StateStore = (*core.MemBackend)(nil)
)
