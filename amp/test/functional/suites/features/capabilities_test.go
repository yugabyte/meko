package features

import (
	"github.com/yugabyte/amp/test/functional/framework/env/sharedk8s"
	"github.com/yugabyte/amp/test/functional/framework/env/universe"
)

// The capability gates in this suite are structural type-asserts, so an env that
// quietly loses a method turns its tests into permanent SKIPS rather than
// failures — a green run that proves nothing. These assertions make that a
// compile error instead.
//
// They are what made deleting the docker single-container env safe: it had been
// the only implementor of storageEnv, and sharedk8s took that over. If one of
// these stops compiling, the fix is to restore the capability on the env, NOT to
// delete the assertion — the alternative is regression-tagged tests that skip
// forever and nobody notices.
//
// sharedk8s is now the only env carrying the full set; universe serves an
// endpoint's Postgres and nothing more.
var (
	_ storageEnv       = (*sharedk8s.Env)(nil)
	_ computeRestarter = (*sharedk8s.Env)(nil)
	_ pgEnv            = (*sharedk8s.Env)(nil)
	_ pgEnv            = (*universe.Env)(nil)
)
