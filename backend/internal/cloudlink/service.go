package cloudlink

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"github.com/yugabyte/meko/backend/internal/agents"
)

// Service changes the link: the web UI's Settings › Meko Cloud and meko cloud_link go through it,
// one change at a time.
type Service struct {
	Store Store
	// Check asks the MCP server at url to take key (CheckMCP); tests swap it out.
	Check func(ctx context.Context, url, key string) error
	// CheckAPI asks whether a REST API URL answers like Meko's (CheckAPI); tests swap it out.
	CheckAPI func(ctx context.Context, apiURL string) error
	// Changed, if set, hears that the link changed (it tells the running extensions).
	Changed func(ctx context.Context)
	// Moved, if set, moves back to this Meko the agents Meko had connected to the link before a saved
	// change (internal/agents' Move); what it did goes in the reply. A failed move never undoes the
	// change.
	Moved func(ctx context.Context, before, after State) agents.Moves
	// List asks a REST API URL for the key's datapacks (ListDatapacks, when nil); tests swap it out.
	List func(ctx context.Context, apiURL, key string) ([]Datapack, error)
	// SignInClient makes a sign-in's requests (signin.go); nil is a plain client.
	SignInClient *http.Client

	mu sync.Mutex
	// signIns is the web UI's sign-in in progress (signin.go).
	signIns signIns
	// state is the link as last read or saved, so a call that asks about it (every tools/call while
	// linked) doesn't read the secret store, which on macOS runs a program each time. gen counts the
	// changes saved, so a call that read the link before one can't use it after (remote.go).
	stateMu sync.Mutex
	state   *State
	gen     int
	// remote is the kept session with the linked Meko Cloud's MCP server (remote.go).
	remote remote
}

// load is the link: as last read or saved, else read now.
func (s *Service) load() (State, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.loadLocked()
}

func (s *Service) loadLocked() (State, error) {
	if s.state != nil {
		return *s.state, nil
	}
	st, err := s.Store.Load()
	if err != nil {
		return State{}, err
	}
	s.state = &st
	return st, nil
}

// saved records a saved link, and drops the session and the tool list of the link before it: a
// call or a read that started before the change finds its link stale (remote.session). The old
// session is closed after stateMu is released, so a slow Meko Cloud never holds up a read of the link.
func (s *Service) saved(st State) { s.replace(&st) }

// unsure forgets the link as last read, after a save that failed partway (the key written but not
// cloud.toml, say): the next read takes it from the store again, as meko token --json does.
func (s *Service) unsure() { s.replace(nil) }

func (s *Service) replace(st *State) {
	s.stateMu.Lock()
	s.state = st
	s.gen++
	old := s.remote.reset(s.gen)
	s.stateMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// Update is a change: a nil field is left as it is. An empty Key keeps the saved one.
type Update struct {
	URL    *string `json:"url"`
	WebURL *string `json:"web_url"`
	APIURL *string `json:"api_url"`
	Key    *string `json:"api_key"`
}

// Status is the link as the web UI and meko status show it: never the key.
type Status struct {
	Set             bool    `json:"set"`    // set once, linked or unlinked: extensions no longer hand over keys
	Linked          bool    `json:"linked"` // a key is saved, and extensions use this link
	URL             string  `json:"url"`    // the MCP server as set; "" is Meko Cloud's
	WebURL          string  `json:"web_url"`
	APIURL          string  `json:"api_url"`
	EffectiveURL    string  `json:"effective_url"`
	EffectiveWebURL *string `json:"effective_web_url"` // null when it can't be worked out
	EffectiveAPIURL *string `json:"effective_api_url"`
	// KeyHint is the key's last four characters, "" for a key too short to hint at, null with none.
	KeyHint       *string `json:"key_hint"`
	DefaultURL    string  `json:"default_url"`
	DefaultWebURL string  `json:"default_web_url"`
	// Agents is what a saved change did to the agents connected to Meko: only in a change's reply.
	Agents *agents.Moves `json:"agents,omitempty"`
}

// Error is a change refused for a reason the person making it can act on.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func refuse(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// StatusOf describes a state.
func StatusOf(st State) Status {
	l := st.Link
	out := Status{Set: st.Set, Linked: st.Linked, URL: l.URL, WebURL: l.WebURL, APIURL: l.APIURL,
		EffectiveURL: l.MCPURL(), EffectiveWebURL: strOrNil(l.EffectiveWebURL()),
		EffectiveAPIURL: strOrNil(l.EffectiveAPIURL()), DefaultURL: DefaultURL, DefaultWebURL: DefaultWebURL}
	if st.Linked {
		hint := ""
		if len(l.Key) >= 12 {
			hint = l.Key[len(l.Key)-4:]
		}
		out.KeyHint = &hint
	}
	return out
}

// Status reads the link.
func (s *Service) Status() (Status, error) {
	st, err := s.load()
	if err != nil {
		return Status{}, err
	}
	return StatusOf(st), nil
}

func normalURL(v *string, what string) (string, error) {
	u := strings.TrimRight(strings.TrimSpace(*v), "/")
	if u == "" {
		return "", nil
	}
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "", refuse("The %s isn't a URL. It should look like https://meko.example.com.", what)
	}
	if p.Scheme != "https" && p.Scheme != "http" {
		return "", refuse("The %s must start with https:// or http://.", what)
	}
	return u, nil
}

// A key Meko can keep (internal/secrets' rule): printable, no spaces, quotes or backslashes.
var validKey = regexp.MustCompile(`^[!#-\[\]-~]+$`)

// originOf is a URL's scheme, host and port, never its credentials; "" when it isn't one.
func originOf(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

// withURLs is the link a change would make, but for its key: the saved URLs with the change's, and
// the saved key only while linked.
func withURLs(cur State, u Update) (Link, error) {
	next := cur.Link
	if !cur.Linked {
		next.Key = ""
	}
	var err error
	if u.URL != nil {
		if next.URL, err = normalURL(u.URL, "MCP server URL"); err != nil {
			return Link{}, err
		}
		if next.URL == DefaultURL {
			next.URL = ""
		}
	}
	if u.WebURL != nil {
		if next.WebURL, err = normalURL(u.WebURL, "web app URL"); err != nil {
			return Link{}, err
		}
	}
	if u.APIURL != nil {
		if next.APIURL, err = normalURL(u.APIURL, "API URL"); err != nil {
			return Link{}, err
		}
	}
	return next, nil
}

// Link checks a change and saves it: a new server or key must answer, a typed web app or API URL
// must lead to an API that answers like Meko's, and a saved key goes only where it already goes or
// to Meko Cloud. Nothing is saved when it's refused (an *Error) or a check fails.
func (s *Service) Link(ctx context.Context, u Update) (Status, error) {
	st, changed, err := s.link(ctx, u)
	if changed {
		s.changed(ctx)
	}
	return st, err
}

// link checks and saves a change under the lock, and reports whether it saved one; the extensions
// are told after the lock is released, so a slow one holds up neither the reply nor the next change.
func (s *Service) link(ctx context.Context, u Update) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.load()
	if err != nil {
		return Status{}, false, err
	}
	next, err := withURLs(cur, u)
	if err != nil {
		return Status{}, false, err
	}
	typedKey := u.Key != nil && strings.TrimSpace(*u.Key) != ""
	if typedKey {
		next.Key = strings.TrimSpace(*u.Key)
		if len(next.Key) > 4096 || !validKey.MatchString(next.Key) {
			return Status{}, false, refuse("A Meko API key has no spaces or quotes in it. Paste it again.")
		}
	}
	if next.Key == "" {
		return Status{}, false, refuse("Enter a Meko Cloud API key.")
	}
	if cur.Linked && next == cur.Link {
		return StatusOf(cur), false, nil
	}
	// A saved key goes only where it already goes, or to Meko Cloud. Sending it elsewhere takes it
	// typed again: otherwise anyone with the browser's login could point Meko at their own server
	// and read the key off the check, or off the extensions' next call.
	if !typedKey {
		known := map[string]bool{}
		for _, o := range []string{cur.Link.MCPURL(), cur.Link.EffectiveAPIURL(), DefaultURL, DefaultWebURL} {
			if o = originOf(o); o != "" {
				known[o] = true
			}
		}
		for _, to := range []string{originOf(next.MCPURL()), originOf(next.EffectiveAPIURL())} {
			if to != "" && !known[to] {
				return Status{}, false, refuse("Enter the Meko API key again to use it with %s: Meko sends a saved key "+
					"only to a server it already uses. Nothing was saved.", to)
			}
		}
	}
	if !cur.Linked || next.MCPURL() != cur.Link.MCPURL() || next.Key != cur.Link.Key {
		if err := s.Check(ctx, next.MCPURL(), next.Key); err != nil {
			return Status{}, false, refuse("Meko at %s didn't accept that key: %v. Nothing was saved.", next.MCPURL(), err)
		}
	}
	// Worked out for Meko Cloud, the API URL is trusted; one that came from a typed URL is checked.
	typedAPI := next.WebURL != cur.Link.WebURL || next.APIURL != cur.Link.APIURL
	if api := next.EffectiveAPIURL(); typedAPI && api != "" && api != cur.Link.EffectiveAPIURL() {
		if err := s.CheckAPI(ctx, api); err != nil {
			return Status{}, false, refuse(
				"%s doesn't look like Meko's API: %v. Check the API or web app URL. Nothing was saved.", api, err)
		}
	}
	if err := s.Store.save(true, next); err != nil {
		s.unsure()
		return Status{}, false, err
	}
	after := State{Set: true, Linked: true, Link: next}
	s.saved(after)
	return s.moved(ctx, cur, after), true, nil
}

// Unlink removes the key, so extensions use the Meko on this computer. The URLs stay, for linking
// again with only a key.
func (s *Service) Unlink(ctx context.Context) (Status, error) {
	st, changed, err := s.unlink(ctx)
	if changed {
		s.changed(ctx)
	}
	return st, err
}

func (s *Service) unlink(ctx context.Context) (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.load()
	if err != nil {
		return Status{}, false, err
	}
	if cur.Set && !cur.Linked {
		return StatusOf(cur), false, nil
	}
	link := cur.Link
	link.Key = ""
	if err := s.Store.save(false, link); err != nil {
		s.unsure()
		return Status{}, false, err
	}
	after := State{Set: true, Link: link}
	s.saved(after)
	return s.moved(ctx, cur, after), true, nil
}

// moved moves the agents after a saved change, still under the lock so the moves happen in the
// changes' order, and describes the link after it with what the move did.
func (s *Service) moved(ctx context.Context, before, after State) Status {
	st := StatusOf(after)
	if s.Moved != nil {
		m := s.Moved(context.WithoutCancel(ctx), before, after)
		st.Agents = &m
	}
	return st
}

func (s *Service) changed(ctx context.Context) {
	if s.Changed != nil {
		s.Changed(context.WithoutCancel(ctx))
	}
}

// MoveAgents moves back to this Meko the agents Meko connected to the linked Meko Cloud before
// agents stayed on this Meko, through Moved: for a Meko linked while its agents followed the link.
// It holds the changes' lock, so it never runs between a change and that change's own move. While
// unlinked, or without Moved, it does nothing.
func (s *Service) MoveAgents(ctx context.Context) (*agents.Moves, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.load()
	if err != nil || !cur.Linked || s.Moved == nil {
		return nil, err
	}
	m := s.Moved(ctx, cur, cur)
	return &m, nil
}
