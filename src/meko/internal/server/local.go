package server

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// forwarded are the headers a proxy adds: the request really came from somewhere else.
var forwarded = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip"}

// LocalAccess reports whether a browser's request over plain HTTP is let in (the rule
// Kode had for its own local access, which Meko took over with its port):
//   - the connection comes from loopback, and Host names this computer (localhost, 127.0.0.1 or
//     [::1]), so a DNS-rebound page, which sends its own host name, gets nothing;
//   - no proxy says it forwarded the request;
//   - another site's page didn't send it (SameSite).
//
// It trusts anything that reaches 127.0.0.1 without those headers: other users of the computer, and
// tunnels or proxies that forward to it.
func LocalAccess(r *http.Request) bool {
	for _, h := range forwarded {
		if len(r.Header.Values(h)) > 0 {
			return false
		}
	}
	if !SameSite(r) {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, err := netip.ParseAddr(host)
	if err != nil || !peer.Unmap().IsLoopback() {
		return false
	}
	name := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		name = h
	}
	switch strings.ToLower(strings.Trim(name, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// SameSite reports whether a browser's request came from no other site's page: the
// browser says Sec-Fetch-Site is same-origin or none (typed in the address bar), and an Origin is
// this one. For a browser that sends Fetch Metadata it does a SameSite=Strict cookie's job: an <img>
// or <script> on another site can't make requests and learn from whether they load. One that
// doesn't (Safari before 16.4, older Firefox) sends neither header on such a GET, so it passes:
// with remote access, the whole check, another site's page in such a browser can make GETs (it
// can't read the replies: no CORS), where the session cookie this check replaced refused them. A
// POST still carries an Origin, which must be this one.
func SameSite(r *http.Request) bool {
	if site := r.Header.Values("Sec-Fetch-Site"); len(site) > 0 && site[0] != "same-origin" && site[0] != "none" {
		return false
	}
	if o := r.Header.Values("Origin"); len(o) > 0 {
		u, err := url.Parse(o[0])
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	return true
}
