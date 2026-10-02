package service

import "strings"

// SentinelAccountID is the all-zeros account the fleet registry defaults to when
// YCP runs without YBM to attribute an environment to (see the column comment in
// 000001_fleet_registry.up.sql).
const SentinelAccountID = "00000000-0000-0000-0000-000000000000"

// MetricsAccountID is the account_id label value an environment's series carry,
// empty when the environment has no real account to attribute them to.
//
// The install hint omits otel.accountId for a sentinel account, so nothing stamps
// the label there. The read path has to agree: matching account_id="0000..." on
// series that carry no account_id at all selects nothing, so a standalone
// environment's charts would come back empty for a reason no error explains.
func MetricsAccountID(accountID string) string {
	trimmed := strings.TrimSpace(accountID)
	if trimmed == SentinelAccountID {
		return ""
	}
	return trimmed
}

// MetricsSource is the source label an environment's series carry: the default of
// otel.byoc.source in its chart — "meko" in the meko chart, "amp" in the amp
// controller's. Anything but meko reads as amp, which is what every environment
// queried before meko shipped metrics was.
func MetricsSource(kind string) string {
	if kind == KindMeko {
		return "meko"
	}
	return "amp"
}
