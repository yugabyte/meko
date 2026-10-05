package yba

// DBVersion resolves the YugabyteDB version a universe (or a release import)
// should use, in the precedence the framework documents at
// framework/configs/default.yaml: an explicit per-app key wins, then the RUN
// override `versions.yb`, then `versions.yugabytedb`.
//
// `versions.yb` is what `mekoqa run -o versions.yb=…` sets, and what the Jenkins
// pipeline passes for every backing (`--yb-version` → `-o versions.yb=`). It has
// to be consulted here: reading only `versions.yugabytedb` made a requested
// version vanish on the yba backing, the universe came up on whatever release
// the YBA bundles, while the run recorded the version the caller asked for. A
// perf comparison or a version-specific repro then draws its conclusion from the
// wrong cluster, with nothing in the result to show for it.
func DBVersion(c ConfigGetter, explicitKey string) string {
	if explicitKey != "" {
		if v := c.GetString(explicitKey, ""); v != "" {
			return v
		}
	}
	if v := c.GetString("versions.yb", ""); v != "" {
		return v
	}
	return c.GetString("versions.yugabytedb", "")
}
