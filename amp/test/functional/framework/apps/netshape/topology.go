package netshape

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Topology is a set of sites and the round trips between them, read from
// "<name>[@<endpoint site>]: <i>-<j>=<rtt ms> ...". Pairs absent are
// co-located. EndpointSite is where a caller puts a pod that is not one of the
// numbered sites (an amp endpoint beside its backing's tservers).
type Topology struct {
	Name         string
	EndpointSite int
	RTT          map[[2]int]time.Duration
}

// ParseTopology reads one topology entry.
func ParseTopology(s string) (Topology, error) {
	head, body, ok := strings.Cut(s, ":")
	if !ok {
		return Topology{}, fmt.Errorf("topology %q: want \"<name>[@<endpoint site>]: <i>-<j>=<rtt ms> ...\"", s)
	}
	tp := Topology{RTT: map[[2]int]time.Duration{}}
	name, site, hasSite := strings.Cut(strings.TrimSpace(head), "@")
	tp.Name = strings.TrimSpace(name)
	if tp.Name == "" {
		return Topology{}, fmt.Errorf("topology %q: the name is empty", s)
	}
	if hasSite {
		n, err := strconv.Atoi(strings.TrimSpace(site))
		if err != nil || n < 0 {
			return Topology{}, fmt.Errorf("topology %q: endpoint site %q is not a non-negative integer", s, site)
		}
		tp.EndpointSite = n
	}
	for _, pair := range strings.Fields(body) {
		sites, ms, ok := strings.Cut(pair, "=")
		a, b, ok2 := strings.Cut(sites, "-")
		if !ok || !ok2 {
			return Topology{}, fmt.Errorf("topology %q: pair %q is not <i>-<j>=<rtt ms>", s, pair)
		}
		i, err1 := strconv.Atoi(a)
		j, err2 := strconv.Atoi(b)
		rtt, err3 := strconv.ParseFloat(ms, 64)
		if err1 != nil || err2 != nil || err3 != nil || i < 0 || j < 0 || i == j || rtt <= 0 {
			return Topology{}, fmt.Errorf("topology %q: pair %q needs two distinct non-negative sites and a positive rtt", s, pair)
		}
		if i > j {
			i, j = j, i
		}
		tp.RTT[[2]int{i, j}] = time.Duration(rtt * float64(time.Millisecond))
	}
	if len(tp.RTT) == 0 {
		return Topology{}, fmt.Errorf("topology %q names no site pair", s)
	}
	return tp, nil
}

// ParseTopologies reads a list of entries, refusing a repeated name.
func ParseTopologies(specs []string) ([]Topology, error) {
	seen := map[string]bool{}
	out := make([]Topology, 0, len(specs))
	for _, s := range specs {
		tp, err := ParseTopology(s)
		if err != nil {
			return nil, err
		}
		if seen[tp.Name] {
			return nil, fmt.Errorf("topology %q is listed twice", tp.Name)
		}
		seen[tp.Name] = true
		out = append(out, tp)
	}
	return out, nil
}

// RoundTrip is the round trip between two sites, 0 when co-located.
func (tp Topology) RoundTrip(a, b int) time.Duration {
	if a == b {
		return 0
	}
	if a > b {
		a, b = b, a
	}
	return tp.RTT[[2]int{a, b}]
}

// MaxSite is the highest site index the topology names.
func (tp Topology) MaxSite() int {
	m := tp.EndpointSite
	for k := range tp.RTT {
		if k[1] > m {
			m = k[1]
		}
	}
	return m
}

// Signature hashes a topology list so a history keyed on it splits when a
// round trip changes and holds when only the pair order does.
func Signature(tps []Topology) string {
	var b strings.Builder
	for _, tp := range tps {
		fmt.Fprintf(&b, "%s@%d:", tp.Name, tp.EndpointSite)
		keys := make([][2]int, 0, len(tp.RTT))
		for k := range tp.RTT {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			return keys[i][0] < keys[j][0] || keys[i][0] == keys[j][0] && keys[i][1] < keys[j][1]
		})
		for _, k := range keys {
			fmt.Fprintf(&b, "%d-%d=%d,", k[0], k[1], tp.RTT[k].Microseconds())
		}
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:8]
}

// jitterSlack is the floor on the tolerance band. It absorbs ping jitter only,
// NOT a round trip: a floor of a millisecond put the lower bound at zero for a
// 1 ms ask, so an unshaped path passed and the two 1 ms topologies verified
// nothing.
const jitterSlack = 500 * time.Microsecond

// Within holds a measured round trip to native + want, the delay being ADDED
// to a path that already costs something: not much below (a direction went
// unshaped) and not far above (something else is in the path). Judging against
// want alone cannot tell a 1 ms ask from the cluster's own sub-millisecond hop.
func Within(got, native, want time.Duration, tolerance float64) error {
	expect := native + want
	slack := time.Duration(float64(want) * tolerance)
	if slack < jitterSlack {
		slack = jitterSlack
	}
	if got < expect-slack || got > expect+slack {
		return fmt.Errorf("measured %s, want %s (%s native + %s injected) within %s",
			got.Round(10*time.Microsecond), expect.Round(10*time.Microsecond), native.Round(10*time.Microsecond), want, slack)
	}
	return nil
}

// CheckSites rejects a pair naming a site that no member occupies. Such a
// delay is never applied (shaping walks members, not sites) and never missed
// (verification walks member pairs), so the run would benchmark an unshaped
// stack and report it under this topology's name.
func (tp Topology) CheckSites(occupied map[int]bool) error {
	var missing []int
	seen := map[int]bool{}
	for k := range tp.RTT {
		for _, site := range k {
			if !occupied[site] && !seen[site] {
				seen[site] = true
				missing = append(missing, site)
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Ints(missing)
	return fmt.Errorf("topology %q names site(s) %v that no pod occupies", tp.Name, missing)
}

// Member is one pod placed in a site.
type Member struct {
	Label  string
	Pod    string
	IP     string
	Site   int
	Helper *Helper
}

// Fleet shapes a set of members into a Topology and measures the result.
type Fleet struct {
	Members []*Member
	Device  string
	// Pings is how many echoes each verification averages; Tolerance is
	// Within's.
	Pings     int
	Tolerance float64
	// native is each pair's round trip with no shaping applied, filled by
	// Baseline and what Verify judges a shaped pair against.
	native   map[[2]string]time.Duration
	baseline []Measurement
}

// Baseline measures every pair's unshaped round trip, which is what Verify
// judges a shaped pair against: an injected delay is ADDED to whatever the
// path already costs, so the native number is what separates a 1 ms ask from
// the cluster's own hop.
//
// It must run before any shaping. Apply calls it for a caller that did not,
// since nothing is shaped yet at that point, and a SECOND call is a no-op
// returning the first measurements — re-measuring would sample a shaped stack
// and quietly move the bar every later check is held to.
func (f *Fleet) Baseline(ctx context.Context) ([]Measurement, error) {
	if f.native != nil {
		return f.baseline, nil
	}
	native := map[[2]string]time.Duration{}
	var out []Measurement
	for i, m := range f.Members {
		for _, o := range f.Members[i+1:] {
			rtt, err := m.Helper.Ping(ctx, o.IP, f.Pings)
			if err != nil {
				return out, err
			}
			native[pairKey(m.Label, o.Label)] = rtt
			out = append(out, Measurement{From: m.Label, To: o.Label, Got: rtt})
		}
	}
	f.native, f.baseline = native, out
	return out, nil
}

// Native is a pair's unshaped round trip, and whether Baseline measured it.
func (f *Fleet) Native(a, b string) (time.Duration, bool) {
	d, ok := f.native[pairKey(a, b)]
	return d, ok
}

// pairKey orders two labels so a pair has one key whichever end names it.
func pairKey(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// Apply installs, on every member, the one-way delay to every member in
// another site: half the pair's round trip, so both ends add up to it. The
// unshaped baseline is measured first when the caller has not done it, which
// is the only moment it can still be measured truthfully.
func (f *Fleet) Apply(ctx context.Context, tp Topology) error {
	if _, err := f.Baseline(ctx); err != nil {
		return err
	}
	for _, m := range f.Members {
		var delays []Delay
		for _, o := range f.Members {
			if o != m {
				delays = append(delays, Delay{Dst: o.IP, OneWay: tp.RoundTrip(m.Site, o.Site) / 2})
			}
		}
		if err := m.Helper.Apply(ctx, f.Device, delays); err != nil {
			return err
		}
	}
	return nil
}

// Measurement is one pair's pinged round trip beside the one asked for and the
// pair's unshaped cost.
type Measurement struct {
	From, To  string
	Got, Want time.Duration
	Native    time.Duration
}

// Verify pings each pair the topology separates ONCE, from the lower member,
// and returns the measurements plus an error naming every pair outside
// tolerance. A round trip is the sum of both directions, so which end sends
// the echo does not matter and an asymmetric shape adding up to the requested
// round trip is accepted — the round trip is what a topology declares.
//
// Baseline must have run: without a pair's native cost there is nothing to
// judge a small ask against, and that is reported rather than assumed.
func (f *Fleet) Verify(ctx context.Context, tp Topology) ([]Measurement, error) {
	var out []Measurement
	var errs []error
	for i, m := range f.Members {
		for _, o := range f.Members[i+1:] {
			want := tp.RoundTrip(m.Site, o.Site)
			if want == 0 {
				continue
			}
			native, ok := f.Native(m.Label, o.Label)
			if !ok {
				return out, fmt.Errorf("no unshaped round trip measured for %s -> %s: Fleet.Baseline must run before any shaping", m.Label, o.Label)
			}
			got, err := m.Helper.Ping(ctx, o.IP, f.Pings)
			if err != nil {
				return out, err
			}
			out = append(out, Measurement{From: m.Label, To: o.Label, Got: got, Want: want, Native: native})
			if err := Within(got, native, want, f.Tolerance); err != nil {
				errs = append(errs, fmt.Errorf("%s -> %s: %w", m.Label, o.Label, err))
			}
		}
	}
	return out, errors.Join(errs...)
}

// VerifyCleared pings every pair the topology delayed and holds each back to
// its native cost, so the next topology starts from a clean stack. Judging the
// pairs the topology shaped (rather than one fixed pair) is the point: two of
// the shipped topologies never delay the tserver-0/tserver-1 pair at all.
func (f *Fleet) VerifyCleared(ctx context.Context, tp Topology) ([]Measurement, error) {
	var out []Measurement
	var errs []error
	for i, m := range f.Members {
		for _, o := range f.Members[i+1:] {
			if tp.RoundTrip(m.Site, o.Site) == 0 {
				continue
			}
			native, ok := f.Native(m.Label, o.Label)
			if !ok {
				return out, fmt.Errorf("no unshaped round trip measured for %s -> %s", m.Label, o.Label)
			}
			got, err := m.Helper.Ping(ctx, o.IP, f.Pings)
			if err != nil {
				return out, err
			}
			out = append(out, Measurement{From: m.Label, To: o.Label, Got: got, Native: native})
			if err := Within(got, native, 0, f.Tolerance); err != nil {
				errs = append(errs, fmt.Errorf("%s -> %s still shaped: %w", m.Label, o.Label, err))
			}
		}
	}
	return out, errors.Join(errs...)
}

// Clear removes the shaping from every member.
func (f *Fleet) Clear(ctx context.Context) error {
	var errs []error
	for _, m := range f.Members {
		if m.Helper != nil {
			errs = append(errs, m.Helper.Clear(ctx, f.Device))
		}
	}
	return errors.Join(errs...)
}

// Close clears every member and deletes the helper pods.
func (f *Fleet) Close(ctx context.Context) {
	for _, m := range f.Members {
		if m.Helper != nil {
			_ = m.Helper.Clear(ctx, f.Device)
			m.Helper.Close(ctx)
		}
	}
}
