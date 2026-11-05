// Package cloudlink keeps the Meko Cloud link: the Meko Cloud (or another Meko server) this Meko's
// extensions use, and the API key they send it. Meko owns it; extensions read it from meko token
// --json and are told when it changes (docs/cloud.md).
//
// The URLs are in cloud.toml beside config.toml, a file Meko writes itself as it does web.toml; the
// key is a secret (internal/secrets), never in a file of its own or a log. cloud.toml existing means
// the link was set, linked or unlinked; an extension may hand Meko a key of its own only before that.
package cloudlink

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
)

// Meko Cloud's MCP server and web app, used when the link names no other.
const (
	DefaultURL    = "https://mcp.mekodata.ai/mcp"
	DefaultWebURL = "https://cloud.mekodata.ai"
)

// SecretName is the key's name in the secret store.
const SecretName = "meko_cloud_key" //nolint:gosec // G101: the secret's name, not a secret

// File is cloud.toml beside config.toml at configPath.
func File(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "cloud.toml")
}

// Link is the saved link. URL is the MCP server, "" for Meko Cloud's; WebURL and APIURL are as set,
// "" to work them out from URL.
type Link struct {
	URL    string `toml:"url"`
	WebURL string `toml:"web_url"`
	APIURL string `toml:"api_url"`
	Key    string `toml:"-"`
}

// MCPURL is the MCP server the link names.
func (l Link) MCPURL() string {
	if l.URL == "" {
		return DefaultURL
	}
	return l.URL
}

// Account names the link's account without giving its key away: the first 16 hex digits of the
// SHA-256 of the MCP URL and the key, "" with no key. A new key or server is another account.
func (l Link) Account() string {
	if l.Key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(l.MCPURL() + "\n" + l.Key))
	return hex.EncodeToString(sum[:8])
}

// EffectiveWebURL is the web app that goes with the link, or "" when it can't be known (a server
// other than Meko Cloud with no web app URL set).
func (l Link) EffectiveWebURL() string { return WebURL(l.MCPURL(), l.WebURL) }

// EffectiveAPIURL is the REST API: as set, else the web app plus /api/v1, else "".
func (l Link) EffectiveAPIURL() string {
	if l.APIURL != "" {
		return l.APIURL
	}
	if w := l.EffectiveWebURL(); w != "" {
		return w + "/api/v1"
	}
	return ""
}

// WebURL is the web app that goes with an MCP URL: Meko Cloud's for Meko Cloud, else "" (another
// server's web app can't be known). An override always wins.
func WebURL(mcpURL, override string) string {
	if o := strings.TrimRight(strings.TrimSpace(override), "/"); o != "" {
		return o
	}
	u, err := url.Parse(mcpURL)
	def, _ := url.Parse(DefaultURL)
	if err != nil || u.Hostname() != def.Hostname() {
		return ""
	}
	return DefaultWebURL
}

// State is what's saved: whether the link was ever set, whether it's linked now, and the link.
type State struct {
	Set    bool
	Linked bool
	Link   Link
}

// Store reads and writes the link.
type Store struct {
	File    string
	Secrets secrets.Store
}

type fileContents struct {
	Linked bool   `toml:"linked"`
	URL    string `toml:"url"`
	WebURL string `toml:"web_url"`
	APIURL string `toml:"api_url"`
}

// Load reads the link. A linked file whose key is gone reads as unlinked.
func (s Store) Load() (State, error) {
	var f fileContents
	_, err := toml.DecodeFile(s.File, &f)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("%s: %w", s.File, err)
	}
	st := State{Set: true, Link: Link{URL: f.URL, WebURL: f.WebURL, APIURL: f.APIURL}}
	if !f.Linked {
		return st, nil
	}
	key, err := s.Secrets.Get(SecretName)
	if errors.Is(err, secrets.ErrNotFound) {
		return st, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("can't read the Meko Cloud key: %w", err)
	}
	st.Linked, st.Link.Key = true, key
	return st, nil
}

// save writes the link: the key first, so a linked file never names a key that isn't there yet.
// Unlinking writes the file first and removes the key after, for the same reason.
func (s Store) save(linked bool, l Link) error {
	if linked {
		if err := s.Secrets.Set(SecretName, l.Key); err != nil {
			return fmt.Errorf("can't save the Meko Cloud key: %w", err)
		}
	}
	var b strings.Builder
	b.WriteString("# The Meko Cloud link, written by Meko (its web UI's Settings › Meko Cloud, or meko cloud_link).\n" +
		"# The API key is kept with Meko's other secrets, not here.\n")
	fmt.Fprintf(&b, "linked = %t\n", linked)
	for _, kv := range [][2]string{{"url", l.URL}, {"web_url", l.WebURL}, {"api_url", l.APIURL}} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s = %q\n", kv[0], kv[1])
		}
	}
	if err := paths.WriteFile(s.File, []byte(b.String())); err != nil {
		return err
	}
	if !linked {
		if err := s.Secrets.Delete(SecretName); err != nil {
			return fmt.Errorf("can't remove the Meko Cloud key: %w", err)
		}
	}
	return nil
}
