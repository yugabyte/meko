// The yb-admin calls a planned-maintenance scenario needs, not a general wrapper.
// The executor runs a shell command on a node with the YB binaries; this package never resolves one itself.
package ybadmin

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/amp/test/functional/framework/exec"
)

const (
	DefaultBinary = "/home/yugabyte/bin/yb-admin"
	DefaultPoll   = 10 * time.Second
)

// Full drains every tablet replica off the node; Leader moves only leadership.
type BlacklistKind string

const (
	BlacklistFull   BlacklistKind = "change_blacklist"
	BlacklistLeader BlacklistKind = "change_leader_blacklist"
)

type TabletServer struct {
	UUID     string
	HostPort string
	Status   string
}

func (ts TabletServer) Alive() bool { return ts.Status == "ALIVE" }

type Client struct {
	ex      exec.RemoteExecutor
	masters string
	binary  string
	logf    func(string, ...any)
}

type Option func(*Client)

func WithBinary(path string) Option          { return func(c *Client) { c.binary = path } }
func WithLogf(f func(string, ...any)) Option { return func(c *Client) { c.logf = f } }

func New(ex exec.RemoteExecutor, masterAddrs string, opts ...Option) *Client {
	c := &Client{ex: ex, masters: masterAddrs, binary: DefaultBinary}
	for _, o := range opts {
		o(c)
	}
	return c
}

func (c *Client) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
	}
}

func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	cmd := c.binary + " --master_addresses " + c.masters + " " + strings.Join(args, " ")
	res, err := c.ex.Run(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("yb-admin %s: %w", args[0], err)
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("yb-admin %s exited %d: %s", args[0], res.ExitCode,
			strings.TrimSpace(firstNonEmpty(res.Stderr, res.Stdout)))
	}
	return res.Stdout, nil
}

func (c *Client) TabletServers(ctx context.Context) ([]TabletServer, error) {
	out, err := c.Run(ctx, "list_all_tablet_servers")
	if err != nil {
		return nil, err
	}
	return parseTabletServers(out), nil
}

func (c *Client) AliveTabletServers(ctx context.Context) ([]TabletServer, error) {
	all, err := c.TabletServers(ctx)
	if err != nil {
		return nil, err
	}
	live := make([]TabletServer, 0, len(all))
	for _, ts := range all {
		if ts.Alive() {
			live = append(live, ts)
		}
	}
	return live, nil
}

// ADD is additive and REMOVE subtractive; the caller drives both ends explicitly.
func (c *Client) Blacklist(ctx context.Context, kind BlacklistKind, op string, hostPorts ...string) error {
	if len(hostPorts) == 0 {
		return fmt.Errorf("ybadmin: %s %s needs at least one host:port", kind, op)
	}
	args := append([]string{string(kind), strings.ToUpper(op)}, hostPorts...)
	c.log("yb-admin %s %s %s", kind, strings.ToUpper(op), strings.Join(hostPorts, " "))
	_, err := c.Run(ctx, args...)
	return err
}

func (c *Client) LoadMoveCompletion(ctx context.Context) (float64, error) {
	out, err := c.Run(ctx, "get_load_move_completion")
	if err != nil {
		return 0, err
	}
	return parseLoadMovePercent(out)
}

// Retries a sample error: masters can be mid-election, and one refused RPC isn't a failed move.
func (c *Client) WaitLoadMove(ctx context.Context, sample func(context.Context) (float64, error), timeout, poll time.Duration) (float64, error) {
	if poll <= 0 {
		poll = DefaultPoll
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last float64
	var lastErr error
	for {
		pct, err := sample(wctx)
		if err == nil {
			last, lastErr = pct, nil
			c.log("load move %.0f%% complete", pct)
			if pct >= 100 {
				return pct, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-wctx.Done():
			if lastErr != nil {
				return last, fmt.Errorf("load move stalled at %.0f%% after %s (last sample: %v)", last, timeout, lastErr)
			}
			return last, fmt.Errorf("load move stalled at %.0f%% after %s", last, timeout)
		case <-time.After(poll):
		}
	}
}

// LoadBalancerIdle accepts both printings yb-admin has used across versions.
func (c *Client) LoadBalancerIdle(ctx context.Context) (bool, error) {
	out, err := c.Run(ctx, "get_is_load_balancer_idle")
	if err != nil {
		return false, err
	}
	m := idleRe.FindStringSubmatch(out)
	if m == nil {
		return false, fmt.Errorf("no idle verdict in yb-admin output: %s", strings.TrimSpace(truncate(out)))
	}
	return m[1] == "1" || strings.EqualFold(m[1], "true"), nil
}

// One reading isn't enough: right after a node rejoins, before the balancer picks up the work, it reads idle too.
const IdleConfirmations = 3

// A sample error resets the streak: a cluster mid-election hasn't settled.
func (c *Client) WaitLoadBalancerIdle(ctx context.Context, timeout, poll time.Duration) error {
	if poll <= 0 {
		poll = DefaultPoll
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	idleStreak := 0
	for {
		idle, err := c.LoadBalancerIdle(wctx)
		if err == nil {
			lastErr = nil
			if idle {
				idleStreak++
				if idleStreak >= IdleConfirmations {
					return nil
				}
			} else {
				idleStreak = 0
			}
		} else {
			lastErr = err
			idleStreak = 0
		}
		select {
		case <-wctx.Done():
			if lastErr != nil {
				return fmt.Errorf("load balancer still busy after %s (last sample: %v)", timeout, lastErr)
			}
			return fmt.Errorf("load balancer still busy after %s", timeout)
		case <-time.After(poll):
		}
	}
}

// StepDownMasterLeader errors when there is no other master to hand it to.
func (c *Client) StepDownMasterLeader(ctx context.Context) error {
	_, err := c.Run(ctx, "master_leader_stepdown")
	return err
}

// Status is found by token, not index: the column set has grown across versions.
func parseTabletServers(out string) []TabletServer {
	var servers []TabletServer
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.Contains(fields[1], ":") {
			continue
		}
		if strings.HasPrefix(fields[0], "Tablet") || !isHex(fields[0]) {
			continue
		}
		ts := TabletServer{UUID: fields[0], HostPort: fields[1]}
		for _, f := range fields[2:] {
			if f == "ALIVE" || f == "DEAD" {
				ts.Status = f
				break
			}
		}
		servers = append(servers, ts)
	}
	return servers
}

var idleRe = regexp.MustCompile(`Idle\s*=\s*(\S+)`)

var loadMoveRe = regexp.MustCompile(`Percent\s+complete\s*=\s*([0-9]+(?:\.[0-9]+)?)`)

// yb-admin prints 100% with nothing blacklisted, so an empty blacklist reads as done.
func parseLoadMovePercent(out string) (float64, error) {
	m := loadMoveRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("no completion percentage in yb-admin output: %s", strings.TrimSpace(truncate(out)))
	}
	return strconv.ParseFloat(m[1], 64)
}

func isHex(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string) string {
	if len(s) <= 400 {
		return s
	}
	return s[:400] + "…"
}
