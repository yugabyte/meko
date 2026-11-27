package cloudlink

import (
	"context"
	"crypto/sha256"
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

// CodeSignInUnsupported is a linked Meko Cloud without the sign-in routes: one from before them.
const CodeSignInUnsupported = "CLOUD_SIGNIN_UNSUPPORTED"

// CodeNoWebURL is a link with no web app URL, where /meko-signin would be.
const CodeNoWebURL = "CLOUD_NO_WEB_URL"

// ErrSignInExpired is a finish that can't complete: its state is unknown, used or past
// BrowserSignInWait, or Meko Cloud refused the code (unknown, expired, used, or not this sign-in's).
// Either way the person signs in again.
var ErrSignInExpired = errors.New("that sign-in has expired: sign in again")

// browserSignIns are the sign-ins waiting for their browsers, by state: one per browser.
type browserSignIns struct {
	mu      sync.Mutex
	pending map[string]browserSignIn
}

type browserSignIn struct {
	verifier string
	returnTo string
	next     string
	started  time.Time
}

// BrowserSignIn is a finished sign-in: whether the person owns the linked account, their email
// when they do, and the page to go to (the next StartBrowserSignIn was given).
type BrowserSignIn struct {
	SameAccount bool
	Email       string
	Next        string
}

// StartBrowserSignIn starts a sign-in whose browser Meko Cloud sends back to returnTo (this Meko's
// own /signin/callback, at its configured address), going to next after, and returns the address
// of Meko Cloud's sign-in page. A *CallError when Meko Cloud isn't linked or the link has no web
// app URL.
func (s *Service) StartBrowserSignIn(_ context.Context, returnTo, next string) (string, error) {
	l, err := s.linked()
	if err != nil {
		return "", err
	}
	web := strings.TrimRight(l.EffectiveWebURL(), "/")
	if web == "" || secureURL(web) != nil {
		return "", &CallError{Code: CodeNoWebURL, msg: "The Meko Cloud link has no https web app URL. " +
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
	b.pending[state] = browserSignIn{verifier: verifier, returnTo: returnTo, next: next, started: time.Now()}
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

// take removes and returns the sign-in started with state, if it's still waiting.
func (b *browserSignIns) take(state string, now time.Time) (browserSignIn, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[state]
	delete(b.pending, state)
	return p, ok && now.Sub(p.started) <= BrowserSignInWait
}

// FinishBrowserSignIn trades the code Meko Cloud sent the browser back with, for the sign-in
// started with state, for whether the person owns the linked account. The sign-in is used up
// whatever the answer. ErrSignInExpired when it can't complete (sign in again); a *CallError with
// CodeSignInUnsupported for a Meko Cloud without the route, or another *CallError when Meko Cloud
// can't be asked.
func (s *Service) FinishBrowserSignIn(ctx context.Context, code, state string) (BrowserSignIn, error) {
	p, ok := s.browserSignIns.take(state, time.Now())
	if !ok || code == "" {
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
	out := BrowserSignIn{SameAccount: reply.SameAccount, Next: p.next}
	if reply.SameAccount {
		out.Email = reply.Email
	}
	return out, nil
}

// unsupported says whether a failed call reached a Meko Cloud without the route: a 404 that isn't
// its API's NOT_FOUND, as internal/cloudsync tells it.
func unsupported(e *CallError) bool {
	return e.cloudStatus == http.StatusNotFound || e.Status == http.StatusNotFound && e.Code != "NOT_FOUND"
}
