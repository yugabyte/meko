package auth

import (
	"net/netip"
	"sync"
	"time"
)

// LoginLimiter counts failed sign-ins in memory, per client network and across all clients, so many
// addresses can't share out a guessing run. Past the site-wide limit everyone waits, the owner too;
// a setup link from meko setup_link still gets them in, since the limit doesn't hold it back.
type LoginLimiter struct {
	Max, SiteMax int           // failures allowed in Window per client network, and in all
	Window       time.Duration // how long a failure counts

	mu       sync.Mutex
	failures map[string][]time.Time
	all      []time.Time
}

// NewLoginLimiter allows 5 failures per client network and 50 in all in 15 minutes.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{Max: 5, SiteMax: 50, Window: 15 * time.Minute, failures: map[string][]time.Time{}}
}

// ClientKey is what failures are counted under: an IPv4 address, or an IPv6 address's /64, since an
// IPv6 client usually holds a whole /64. An IPv4 address in IPv6 form counts as the IPv4 address.
func ClientKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is4() {
		return a.String()
	}
	p, _ := a.Prefix(64)
	return p.String()
}

// Blocked reports whether ip may not try now.
func (l *LoginLimiter) Blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = recent(l.all, now, l.Window)
	return len(l.recentFor(ClientKey(ip), now)) >= l.Max || len(l.all) >= l.SiteMax
}

// Take counts an attempt from ip as failed before it's checked, unless ip may not try now, in one
// step: requests that arrive together can't all get past Blocked before any of them fails. An
// attempt that turns out right takes its count back with Refund.
func (l *LoginLimiter) Take(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = recent(l.all, now, l.Window)
	k := ClientKey(ip)
	r := l.recentFor(k, now)
	if len(r) >= l.Max || len(l.all) >= l.SiteMax {
		return false
	}
	l.failures[k] = append(r, now)
	l.all = append(l.all, now)
	return true
}

// Refund takes back the count Take made at now for ip.
func (l *LoginLimiter) Refund(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := ClientKey(ip)
	l.failures[k] = dropOne(l.failures[k], now)
	if len(l.failures[k]) == 0 {
		delete(l.failures, k)
	}
	l.all = dropOne(l.all, now)
}

func dropOne(ts []time.Time, t time.Time) []time.Time {
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(t) {
			return append(ts[:i], ts[i+1:]...)
		}
	}
	return ts
}

// Fail counts a failed attempt from ip.
func (l *LoginLimiter) Fail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := ClientKey(ip)
	l.failures[k] = append(l.recentFor(k, now), now)
	l.all = append(l.all, now)
}

// Reset clears ip's count after it signs in. The site-wide count only runs out.
func (l *LoginLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ClientKey(ip))
}

func (l *LoginLimiter) recentFor(key string, now time.Time) []time.Time {
	r := recent(l.failures[key], now, l.Window)
	if len(r) == 0 {
		delete(l.failures, key)
	} else {
		l.failures[key] = r
	}
	return r
}

func recent(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if now.Sub(t) < window {
			out = append(out, t)
		}
	}
	return out
}
