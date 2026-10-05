// Package server is the Cognito user pool mock for the mekoqa stack.
//
// Two listeners, see Serve. The public one is the surface the api-server reaches
// as cognito-idp.<region>.amazonaws.com: the Cognito JSON API (X-Amz-Target)
// and the pool's JWKS. The control one carries the framework's control API and
// the health probe.
//
// The pool is in memory and mirrored to Config.StatePath, so a restarted
// container keeps its users. Tokens are RS256 signed with Config.Key.
package server

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const symbols = "^$*.[]{}()?-\"!@#%&/\\,><':;|_~`+="

type Config struct {
	Region           string
	PoolID           string
	ClientID         string
	ClientSecret     string
	TokenTTL         time.Duration
	ConfirmationCode string
	KID              string
	Key              *rsa.PrivateKey
	StatePath        string
	Logf             func(string, ...any)
}

func (c Config) withDefaults() Config {
	if c.TokenTTL <= 0 {
		c.TokenTTL = 24 * time.Hour
	}
	if c.ConfirmationCode == "" {
		c.ConfirmationCode = "123456"
	}
	if c.KID == "" {
		c.KID = "mekoqa-1"
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return c
}

type user struct {
	Username string            `json:"username"`
	Sub      string            `json:"sub"`
	Password string            `json:"password"`
	Status   string            `json:"status"`
	Attrs    map[string]string `json:"attrs"`
	Created  float64           `json:"created"`
}

type cognitoError struct{ kind, message string }

func (e *cognitoError) Error() string { return e.kind + ": " + e.message }

func fail(kind, format string, args ...any) error {
	return &cognitoError{kind, fmt.Sprintf(format, args...)}
}

type Pool struct {
	cfg Config
	mu  sync.Mutex

	users    map[string]*user
	sessions map[string]string
	refresh  map[string]string
}

func New(cfg Config) (*Pool, error) {
	cfg = cfg.withDefaults()
	if cfg.Key == nil {
		return nil, errors.New("cognito mock: no token signing key")
	}
	if cfg.Region == "" || cfg.PoolID == "" || cfg.ClientID == "" {
		return nil, errors.New("cognito mock: region, pool id and client id are required")
	}
	p := &Pool{cfg: cfg, users: map[string]*user{}, sessions: map[string]string{}, refresh: map[string]string{}}
	p.load()
	return p, nil
}

func (p *Pool) Issuer() string {
	return fmt.Sprintf("https://cognito-idp.%s.amazonaws.com/%s", p.cfg.Region, p.cfg.PoolID)
}

func (p *Pool) load() {
	if p.cfg.StatePath == "" {
		return
	}
	raw, err := os.ReadFile(p.cfg.StatePath)
	if err != nil {
		return
	}
	var saved struct {
		Users   map[string]*user  `json:"users"`
		Refresh map[string]string `json:"refresh"`
	}
	if json.Unmarshal(raw, &saved) != nil {
		return
	}
	if saved.Users != nil {
		p.users = saved.Users
	}
	if saved.Refresh != nil {
		p.refresh = saved.Refresh
	}
}

func (p *Pool) save() {
	if p.cfg.StatePath == "" {
		return
	}
	raw, err := json.Marshal(map[string]any{"users": p.users, "refresh": p.refresh})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(p.cfg.StatePath), 0o755)
	}
	if err == nil {
		tmp := p.cfg.StatePath + ".tmp"
		if err = os.WriteFile(tmp, raw, 0o600); err == nil {
			err = os.Rename(tmp, p.cfg.StatePath)
		}
	}
	if err != nil {
		p.cfg.Logf("state not saved: %v", err)
	}
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func uuidHex() string { return strings.ReplaceAll(uuid4(), "-", "") }

func tempPassword() string { return "Aa1!" + uuidHex()[:12] }

func now() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func (p *Pool) ordered() []*user {
	out := make([]*user, 0, len(p.users))
	for _, u := range p.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].Username < out[j].Username
	})
	return out
}

func key(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

func (p *Pool) find(username string) (*user, error) {
	u := p.users[key(username)]
	if u == nil {
		return nil, fail("UserNotFoundException", "User does not exist.")
	}
	return u, nil
}

func (p *Pool) newUser(username, password string, attrs map[string]string, status string) (*user, error) {
	k := key(username)
	if _, ok := p.users[k]; ok {
		return nil, fail("UsernameExistsException", "User already exists")
	}
	cp := make(map[string]string, len(attrs)+1)
	for name, v := range attrs {
		cp[name] = v
	}
	if _, ok := cp["email"]; !ok {
		cp["email"] = username
	}
	u := &user{Username: username, Sub: uuid4(), Password: password, Status: status, Attrs: cp, Created: now()}
	p.users[k] = u
	return u, nil
}
