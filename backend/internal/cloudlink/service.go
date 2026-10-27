package cloudlink

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
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
	// List asks a REST API URL for the key's datapacks (ListDatapacks, when nil); tests swap it out.
	List func(ctx context.Context, apiURL, key string) ([]Datapack, error)

	mu sync.Mutex
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
	st, err := s.Store.Load()
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
	cur, err := s.Store.Load()
	if err != nil {
		return Status{}, false, err
	}
	next := cur.Link
	if !cur.Linked {
		next.Key = ""
	}
	if u.URL != nil {
		if next.URL, err = normalURL(u.URL, "MCP server URL"); err != nil {
			return Status{}, false, err
		}
		if next.URL == DefaultURL {
			next.URL = ""
		}
	}
	if u.WebURL != nil {
		if next.WebURL, err = normalURL(u.WebURL, "web app URL"); err != nil {
			return Status{}, false, err
		}
	}
	if u.APIURL != nil {
		if next.APIURL, err = normalURL(u.APIURL, "API URL"); err != nil {
			return Status{}, false, err
		}
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
		return Status{}, false, err
	}
	return StatusOf(State{Set: true, Linked: true, Link: next}), true, nil
}

// Unlink removes the key, so extensions use the Meko on this computer. The URLs stay, for linking
// again with only a key.
func (s *Service) Unlink(ctx context.Context) (Status, error) {
	st, changed, err := s.unlink()
	if changed {
		s.changed(ctx)
	}
	return st, err
}

func (s *Service) unlink() (Status, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.Store.Load()
	if err != nil {
		return Status{}, false, err
	}
	if cur.Set && !cur.Linked {
		return StatusOf(cur), false, nil
	}
	link := cur.Link
	link.Key = ""
	if err := s.Store.save(false, link); err != nil {
		return Status{}, false, err
	}
	return StatusOf(State{Set: true, Link: link}), true, nil
}

func (s *Service) changed(ctx context.Context) {
	if s.Changed != nil {
		s.Changed(context.WithoutCancel(ctx))
	}
}
