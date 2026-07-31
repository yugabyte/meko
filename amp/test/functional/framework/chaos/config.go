package chaos

import (
	"strconv"
	"time"
)

// Event is an observation callback payload: one inject or recover of one action
// on one target. Emitted so a scenario can fold chaos into its report/steps
// without the engine importing the reporter. Never retained by the engine.
type Event struct {
	Action string
	Target string
	Phase  string // "inject" or "recover"
	Err    error  // nil on success
	At     time.Time
}

// Config holds the chaos engine's cadence, severity, and action-selection knobs.
// These live as code defaults for now (DefaultConfig); a later scenario agent
// wires the corresponding keys into configs/default.yaml. Nothing here is read
// from the framework config directly, so the package stays standalone.
type Config struct {
	// Actions restricts the loop to these action names (in this order, before
	// seed-driven selection). Empty means "every registered action", ordered
	// deterministically so selection stays reproducible.
	Actions []string

	// OwnerPrefix guards which resources may be targeted. A pod target whose
	// namespace lacks this prefix is refused (the loop will not start). Default
	// "ampqa-".
	OwnerPrefix string

	// NetDevice is the interface tc/iptables actions operate on. Default "eth0".
	NetDevice string
	// PartitionPorts are the TCP ports NetworkPartition drops (YB RPC/SQL/CQL).
	PartitionPorts []int
	// DelayMs is the added one-way latency for NetworkSlowdown.
	DelayMs int
	// LossPercent is the drop rate for PacketLoss.
	LossPercent int

	// ProcessPattern matches the process ProcessKill/ProcessRestart signal
	// (pkill -f). Default "yb-tserver".
	ProcessPattern string
	// ServiceUnit is the systemd unit ProcessRestart restarts and ProcessKill
	// recovers on a host. Default "yb-tserver".
	ServiceUnit string
	// NodeUnits are the systemd units NodeRestart restarts on a host.
	NodeUnits []string

	// HoldMin/HoldMax bound how long a fault stays applied before recovery.
	HoldMin, HoldMax time.Duration
	// GapMin/GapMax bound the quiet interval between recovery and the next
	// injection.
	GapMin, GapMax time.Duration

	// WaitTimeout bounds "wait until healthy again" recovery steps (pod Ready,
	// process back, VM running).
	WaitTimeout time.Duration
	// RecoverTimeout bounds each Recover call's own background context, so
	// recovery survives cancellation of the run.
	RecoverTimeout time.Duration

	// Observe, if set, receives an Event on every inject and recover.
	Observe func(Event)
}

// DefaultConfig returns the built-in chaos defaults. Severity is deliberately
// moderate and cadence generous so a background loop disrupts a workload without
// permanently wedging it.
func DefaultConfig() Config {
	return Config{
		OwnerPrefix:    "ampqa-",
		NetDevice:      "eth0",
		PartitionPorts: []int{7100, 9100, 5433, 9042},
		DelayMs:        200,
		LossPercent:    30,
		ProcessPattern: "yb-tserver",
		ServiceUnit:    "yb-tserver",
		NodeUnits:      []string{"yb-master", "yb-tserver"},
		HoldMin:        15 * time.Second,
		HoldMax:        45 * time.Second,
		GapMin:         20 * time.Second,
		GapMax:         60 * time.Second,
		WaitTimeout:    3 * time.Minute,
		RecoverTimeout: 2 * time.Minute,
	}
}

// Getter is the minimal typed-config surface FromConfig reads (structurally
// satisfied by *core.Config, so config.go imports no core config type — the
// apps/* convention, keeping the engine standalone).
type Getter interface {
	GetString(key, def string) string
	GetInt(key string, def int) int
	GetDuration(key string, def time.Duration) time.Duration
	GetStringSlice(key string, def []string) []string
}

// FromConfig reads chaos.* into a Config, defaulting every knob to DefaultConfig
// (the one code default; configs/default.yaml carries the same values). An empty
// chaos.actions means "every registered action" (the loop's default).
func FromConfig(get Getter) Config {
	d := DefaultConfig()
	return Config{
		Actions:        get.GetStringSlice("chaos.actions", nil),
		OwnerPrefix:    get.GetString("chaos.owner_prefix", d.OwnerPrefix),
		NetDevice:      get.GetString("chaos.net_device", d.NetDevice),
		PartitionPorts: getIntSlice(get, "chaos.partition_ports", d.PartitionPorts),
		DelayMs:        get.GetInt("chaos.delay_ms", d.DelayMs),
		LossPercent:    get.GetInt("chaos.loss_percent", d.LossPercent),
		ProcessPattern: get.GetString("chaos.process_pattern", d.ProcessPattern),
		ServiceUnit:    get.GetString("chaos.service_unit", d.ServiceUnit),
		NodeUnits:      get.GetStringSlice("chaos.node_units", d.NodeUnits),
		HoldMin:        get.GetDuration("chaos.hold_min", d.HoldMin),
		HoldMax:        get.GetDuration("chaos.hold_max", d.HoldMax),
		GapMin:         get.GetDuration("chaos.gap_min", d.GapMin),
		GapMax:         get.GetDuration("chaos.gap_max", d.GapMax),
		WaitTimeout:    get.GetDuration("chaos.wait_timeout", d.WaitTimeout),
		RecoverTimeout: get.GetDuration("chaos.recover_timeout", d.RecoverTimeout),
	}
}

// getIntSlice reads a list-of-ints config key (stored as strings/numbers) into
// []int, falling back to def if the key is absent or any element is not an int.
func getIntSlice(get Getter, key string, def []int) []int {
	raw := get.GetStringSlice(key, nil)
	if raw == nil {
		return def
	}
	out := make([]int, 0, len(raw))
	for _, s := range raw {
		n, err := strconv.Atoi(s)
		if err != nil {
			return def
		}
		out = append(out, n)
	}
	return out
}

// withDefaults fills any zero field of c from DefaultConfig so a caller can pass
// a partial Config (e.g. only Actions and Observe).
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.OwnerPrefix == "" {
		c.OwnerPrefix = d.OwnerPrefix
	}
	if c.NetDevice == "" {
		c.NetDevice = d.NetDevice
	}
	if len(c.PartitionPorts) == 0 {
		c.PartitionPorts = d.PartitionPorts
	}
	if c.DelayMs == 0 {
		c.DelayMs = d.DelayMs
	}
	if c.LossPercent == 0 {
		c.LossPercent = d.LossPercent
	}
	if c.ProcessPattern == "" {
		c.ProcessPattern = d.ProcessPattern
	}
	if c.ServiceUnit == "" {
		c.ServiceUnit = d.ServiceUnit
	}
	if len(c.NodeUnits) == 0 {
		c.NodeUnits = d.NodeUnits
	}
	if c.HoldMin == 0 {
		c.HoldMin = d.HoldMin
	}
	if c.HoldMax == 0 {
		c.HoldMax = d.HoldMax
	}
	if c.GapMin == 0 {
		c.GapMin = d.GapMin
	}
	if c.GapMax == 0 {
		c.GapMax = d.GapMax
	}
	if c.WaitTimeout == 0 {
		c.WaitTimeout = d.WaitTimeout
	}
	if c.RecoverTimeout == 0 {
		c.RecoverTimeout = d.RecoverTimeout
	}
	return c
}
