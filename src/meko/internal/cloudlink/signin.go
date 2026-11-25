package cloudlink

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// SignInWait is how long a sign-in the web UI started waits for the browser to come back.
const SignInWait = 10 * time.Minute

// signIns is the web UI's sign-in in progress (one at a time: a new one replaces it), and the
// clients registered with each server for each redirect URI, so signing in again registers none.
type signIns struct {
	mu      sync.Mutex
	pending *pendingSignIn
	clients map[string]OAuthClient
}

type pendingSignIn struct {
	flow    *Flow
	update  Update // the link's URLs, pinned (SignInTarget); the key is the one the sign-in makes
	expires time.Time
}

// SignInTarget is the MCP server a link with u's URLs would use, and so the one a sign-in signs in
// to, with u pinned to it: every URL set as the link would have it now. Linking with the pinned
// change sends the key the sign-in makes to that server, even if the saved link changes meanwhile.
func (s *Service) SignInTarget(u Update) (Update, string, error) {
	cur, err := s.load()
	if err != nil {
		return Update{}, "", err
	}
	next, err := withURLs(cur, u)
	if err != nil {
		return Update{}, "", err
	}
	return Update{URL: &next.URL, WebURL: &next.WebURL, APIURL: &next.APIURL}, next.MCPURL(), nil
}

// StartSignIn starts the web UI's sign-in to the MCP server a link with u's URLs would use, with
// Meko Cloud sending the browser back to redirectURI, and returns where the browser goes to sign
// in. A sign-in already started is dropped.
func (s *Service) StartSignIn(ctx context.Context, u Update, redirectURI string) (string, error) {
	u, mcpURL, err := s.SignInTarget(u)
	if err != nil {
		return "", err
	}
	cacheKey := mcpURL + "\n" + redirectURI
	s.signIns.mu.Lock()
	var client *OAuthClient
	if c, ok := s.signIns.clients[cacheKey]; ok {
		client = &c
	}
	s.signIns.mu.Unlock()
	flow, err := StartFlow(ctx, s.SignInClient, mcpURL, redirectURI, client)
	if err != nil {
		return "", err
	}
	s.signIns.mu.Lock()
	defer s.signIns.mu.Unlock()
	if s.signIns.clients == nil {
		s.signIns.clients = map[string]OAuthClient{}
	}
	s.signIns.clients[cacheKey] = flow.Client()
	s.signIns.pending = &pendingSignIn{flow: flow, update: u, expires: time.Now().Add(SignInWait)}
	return flow.AuthorizeURL, nil
}

// FinishSignIn takes the code and state Meko Cloud sent the browser back with, makes an API key
// with them, and links with it as a typed key (Link), to the server the sign-in signed in to. A
// finish with the sign-in's own state uses it up, whether it links or not; one with another state
// leaves it waiting.
func (s *Service) FinishSignIn(ctx context.Context, code, state string) (Status, error) {
	s.signIns.mu.Lock()
	p := s.signIns.pending
	if p != nil && p.flow.state == state {
		s.signIns.pending = nil
	}
	s.signIns.mu.Unlock()
	if p == nil || time.Now().After(p.expires) {
		return Status{}, refuse("That sign-in has expired. Sign in again.")
	}
	m, err := p.flow.Finish(ctx, code, state, KeyName(time.Now()))
	if err != nil {
		return Status{}, err
	}
	u := p.update
	u.Key = &m.Key
	st, err := s.Link(ctx, u)
	var refused *Error
	switch {
	case err == nil:
		return st, nil
	case errors.As(err, &refused):
		return st, KeyLeft(err, m.Name)
	}
	// Not a refusal (the secret store, the data folder): an internal error, which the route logs and
	// answers generically. Its log line names the key left in the account.
	return st, fmt.Errorf("linking with the key Meko made, %q, which is still in the Meko Cloud account: %w",
		m.Name, err)
}
