package proc

import (
	"os"
	"strings"
)

// envAllowlist is the set of parent-environment variables that are forwarded to
// a spawned child. Everything else in the launcher's environment is scrubbed;
// see buildChildEnv. Mirrors fill_rust_env_vars in
// control_plane/src/background_process.rs.
var envAllowlist = []string{
	"LLVM_PROFILE_FILE",
	"FAILPOINTS",
	"RUST_LOG",
	"ASAN_OPTIONS",
	"UBSAN_OPTIONS",
	// Per-rel-block write-path trace filter (yb_ps_trace target); unset in
	// normal operation, used to debug page-write loss.
	"YB_PS_TRACE_REL_BLK",
	// Needed so children (initdb, postgres, pg_ctl, ...) can find libs shipped
	// alongside a relocatable bundle.
	"LD_LIBRARY_PATH",
	"DYLD_LIBRARY_PATH",
}

// ybEnvPrefix marks environment variables (e.g. YB_STORAGE_PASSWORD) that are
// forwarded wholesale from the launcher to the child.
const ybEnvPrefix = "YB_"

// buildChildEnv constructs the environment for a spawned child from scratch. We
// deliberately do NOT inherit the launcher's full environment; instead we
// re-inject only a curated set, mirroring fill_rust_env_vars +
// fill_env_vars_prefixed_yb in background_process.rs:
//
//   - RUST_BACKTRACE: passed through if set, otherwise defaulted to "1".
//   - the allowlist above, each forwarded only if present in the parent.
//   - every variable whose name begins with "YB_".
//   - the caller-supplied extraEnv, applied last so it overrides the above.
func buildChildEnv(extraEnv map[string]string) []string {
	env := map[string]string{}

	// RUST_BACKTRACE: pass through if set, else default to "1".
	if v, ok := os.LookupEnv("RUST_BACKTRACE"); ok {
		env["RUST_BACKTRACE"] = v
	} else {
		env["RUST_BACKTRACE"] = "1"
	}

	for _, name := range envAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}

	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, ybEnvPrefix) {
			continue
		}
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}

	// Caller-supplied env wins over everything above.
	for k, v := range extraEnv {
		env[k] = v
	}

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
