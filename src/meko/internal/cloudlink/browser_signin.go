package cloudlink

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Signing in to this Meko's remote access through the linked Meko Cloud ([web] signin = "cloud",
// docs/cloud.md): StartBrowserSignIn sends the browser to Meko Cloud's /meko-signin page with a PKCE
// challenge; Meko Cloud sends it back to returnTo with a one-time code; FinishBrowserSignIn trades
// the code, the verifier and the link's key for whether the person who signed in owns the linked
// account. Meko learns nothing else, and keeps no credential of theirs. This is apart from signin.go,
// which links Meko by signing in.

// BrowserSignInWait is how long a started sign-in waits for the browser to come back.
const BrowserSignInWait = 10 * time.Minute

// maxBrowserSignIns bounds the sign-ins waiting at once: starting one needs no credential, so a
// new one drops the oldest past this.
const maxBrowserSignIns = 32

// CodeSignInUnsupported is a linked Meko Cloud without the exchange route: one from before it. Such
// a Meko Cloud has no /meko-signin page either, so a browser sent there sees its not-found page and
// never comes back; finish meets this only from a Meko Cloud that lost the route after start.
const CodeSignInUnsupported = "CLOUD_SIGNIN_UNSUPPORTED"

// CodeNoWebURL is a link with no web app URL, where /meko-signin would be.
const CodeNoWebURL = "CLOUD_NO_WEB_URL"

// ErrSignInExpired is a finish that can't complete: its state is unknown, used or past
// BrowserSignInWait, it came from another browser than the one that started it, the Meko Cloud link
// changed since it started, or Meko Cloud refused the code (unknown, expired, used, or not this
// sign-in's). Either way the person signs in again.
var ErrSignInExpired = errors.New("that sign-in has expired: sign in again")

// browserSignIns are the sign-ins waiting for their browsers, by state: one per browser.
type browserSignIns struct {
	mu      sync.Mutex
	pending map[string]browserSignIn
}

type browserSignIn struct {
	started  time.Time
	verifier string
	returnTo string
	next     string
	browser  string // the hash of the nonce the starting browser holds (StartBrowserSignIn's browser)
	gen      int    // the link's generation at start (Service.gen)
}

// BrowserSignIn is a finished sign-in: whether the person owns the linked account, their email
// when they do, and the page to go to (the next StartBrowserSignIn was given).
type BrowserSignIn struct {
	Email       string
	Next        string
	gen         int
	SameAccount bool
}

// StartBrowserSignIn starts a sign-in whose browser Meko Cloud sends back to returnTo (this Meko's
// own /signin/callback, at its configured address), going to next after, and returns the address
// of Meko Cloud's sign-in page. browser identifies the browser starting it (the hash of a nonce
// only that browser holds): finish takes the code only with the same one, so a callback address
// copied before it's used signs no one else in. A *CallError when Meko Cloud isn't linked or the
// link has no web app URL.
func (s *Service) StartBrowserSignIn(_ context.Context, returnTo, next, browser string) (string, error) {
	s.stateMu.Lock()
	gen := s.gen
	s.stateMu.Unlock()
	l, err := s.linked()
	if err != nil {
		return "", err
	}
	web := strings.TrimRight(l.EffectiveWebURL(), "/")
	if web == "" || secureURL(web) != nil {
		return "", &CallError{Status: http.StatusConflict, Code: CodeNoWebURL,
			msg: "The Meko Cloud link has no https web app URL. " +
				"Set it in Settings › Meko Cloud, or meko cloud-link --web-url."}
	}
	state, verifier := randomString(32), randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	b := &s.browserSignIns
	b.mu.Lock()
	b.drop(time.Now())
	if b.pending == nil {
		b.pending = map[string]browserSignIn{}
	}
	b.pending[state] = browserSignIn{verifier: verifier, returnTo: returnTo, next: next, browser: browser, gen: gen,
		started: time.Now()}
	b.mu.Unlock()
	q := url.Values{"return_to": {returnTo}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	return web + "/meko-signin?" + q.Encode(), nil
}

// drop removes the sign-ins past BrowserSignInWait, then the oldest until there's room for one
// more. b.mu is held.
func (b *browserSignIns) drop(now time.Time) {
	for st, p := range b.pending {
		if now.Sub(p.started) > BrowserSignInWait {
			delete(b.pending, st)
		}
	}
	for len(b.pending) >= maxBrowserSignIns {
		oldest := ""
		for st, p := range b.pending {
			if oldest == "" || p.started.Before(b.pending[oldest].started) {
				oldest = st
			}
		}
		delete(b.pending, oldest)
	}
}

// forget drops every sign-in waiting: the link they started under is gone.
func (b *browserSignIns) forget() {
	b.mu.Lock()
	b.pending = nil
	b.mu.Unlock()
}

// take removes and returns the sign-in started with state, if it's still waiting.
func (b *browserSignIns) take(state string, now time.Time) (browserSignIn, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[state]
	delete(b.pending, state)
	return p, ok && now.Sub(p.started) <= BrowserSignInWait
}

// FinishBrowserSignIn trades the code Meko Cloud sent the browser back with, for the sign-in
// started with state by the same browser, for whether the person owns the linked account. The
// sign-in is used up whatever the answer. ErrSignInExpired when it can't complete (sign in again);
// a *CallError with CodeSignInUnsupported for a Meko Cloud without the route, or another *CallError
// when Meko Cloud can't be asked. Before keeping what it answers, check BrowserSignInCurrent.
func (s *Service) FinishBrowserSignIn(ctx context.Context, code, state, browser string) (BrowserSignIn, error) {
	p, ok := s.browserSignIns.take(state, time.Now())
	if !ok || code == "" || browser == "" || subtle.ConstantTimeCompare([]byte(browser), []byte(p.browser)) != 1 {
		return BrowserSignIn{}, ErrSignInExpired
	}
	data, err := s.API(ctx, http.MethodPost, "/auth/meko-signin/exchange",
		map[string]string{"code": code, "code_verifier": p.verifier, "return_to": p.returnTo})
	var failed *CallError
	switch {
	case err == nil:
	case errors.As(err, &failed) && unsupported(failed):
		return BrowserSignIn{}, &CallError{Status: http.StatusConflict, Code: CodeSignInUnsupported,
			msg: "This Meko Cloud can't sign you in to Meko yet. Use Meko on its own computer, " +
				"or turn sign-in off there."}
	case errors.As(err, &failed) && failed.Status == http.StatusBadRequest && failed.Code == "INVALID_SIGNIN_CODE":
		return BrowserSignIn{}, ErrSignInExpired
	default:
		return BrowserSignIn{}, err
	}
	var reply struct {
		SameAccount bool   `json:"same_account"`
		Email       string `json:"email"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return BrowserSignIn{}, &CallError{Code: CodeCallFailed,
			msg: "Meko Cloud's answer to a sign-in isn't one Meko reads."}
	}
	out := BrowserSignIn{SameAccount: reply.SameAccount, Next: p.next, gen: p.gen}
	if !s.BrowserSignInCurrent(out) {
		return BrowserSignIn{}, ErrSignInExpired
	}
	if reply.SameAccount {
		out.Email = reply.Email
	}
	return out, nil
}

// BrowserSignInCurrent says whether the link b was started under is still the one saved: a change
// since then (a new owner, or none) voids it. A session made from b is kept only if this still
// holds once it's stored: a change saved after that check ends the session with all the others
// (Changed), and one saved before it is caught by it.
func (s *Service) BrowserSignInCurrent(b BrowserSignIn) bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return b.gen == s.gen
}

// unsupported says whether a failed call reached a Meko Cloud without the route: a 404 that isn't
// its API's NOT_FOUND, as internal/cloudsync tells it.
func unsupported(e *CallError) bool {
	return e.cloudStatus == http.StatusNotFound || e.Status == http.StatusNotFound && e.Code != "NOT_FOUND"
}
