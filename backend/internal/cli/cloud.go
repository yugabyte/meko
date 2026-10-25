package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yugabyte/meko/backend/internal/cloudlink"
	"github.com/yugabyte/meko/backend/internal/config"
	"github.com/yugabyte/meko/backend/internal/envelope"
	"github.com/yugabyte/meko/backend/internal/paths"
	"github.com/yugabyte/meko/backend/internal/secrets"
)

// cloudFlags are meko cloud_link's.
type cloudFlags struct {
	url, webURL, apiURL string
	off                 bool
	set                 map[string]bool // the flags given, so --url "" (Meko Cloud's) differs from no --url
}

// cloudTimeout bounds a link through the server: its checks take up to 10 s each.
const cloudTimeout = 30 * time.Second

func cloudStore(l paths.Layout, cfg config.Config) (cloudlink.Store, error) {
	s, err := secrets.Open(cfg.Secrets.Store, l.Root)
	if err != nil {
		return cloudlink.Store{}, err
	}
	return cloudlink.Store{File: cloudlink.File(l.Config), Secrets: s}, nil
}

// runCloudLink links Meko Cloud, or another Meko server, for the extensions: the key from stdin, or
// typed at a hidden prompt; --off unlinks. With a server running it goes through it (the admin
// token), so the running extensions hear of it at once; otherwise it saves the link itself, and they
// read it when they next ask Meko.
func runCloudLink(l paths.Layout, f cloudFlags, in io.Reader, stdout, stderr io.Writer) int {
	if err := l.Ensure(); err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return fail(stderr, err)
	}
	store, err := cloudStore(l, cfg)
	if err != nil {
		return fail(stderr, err)
	}
	cur, err := store.Load()
	if err != nil {
		return fail(stderr, err)
	}
	var u cloudlink.Update
	if !f.off {
		if f.set["url"] {
			u.URL = &f.url
		}
		if f.set["web-url"] {
			u.WebURL = &f.webURL
		}
		if f.set["api-url"] {
			u.APIURL = &f.apiURL
		}
		prompt := "Meko Cloud API key: "
		if cur.Linked {
			prompt = "Meko Cloud API key (Enter keeps the saved one): "
		}
		key, rerr := readSecret(in, stderr, prompt)
		if rerr != nil {
			return fail(stderr, rerr)
		}
		u.Key = &key
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()
	st, err := applyCloud(ctx, l, store, f.off, u)
	if err != nil {
		return fail(stderr, err)
	}
	if !st.Linked {
		fmt.Fprintln(stdout, "Meko Cloud is unlinked: extensions use this Meko")
		return exitOK
	}
	fmt.Fprintln(stdout, "Linked to "+cloudLine(st))
	return exitOK
}

// applyCloud makes the change through the running server, or here when none runs.
func applyCloud(ctx context.Context, l paths.Layout, store cloudlink.Store, off bool, u cloudlink.Update) (
	cloudlink.Status, error) {
	up, err := running(l.Lock)
	if err != nil {
		return cloudlink.Status{}, err
	}
	if up {
		c, cerr := newClient(l)
		if cerr != nil {
			return cloudlink.Status{}, fmt.Errorf("the server is running, but %w", cerr)
		}
		c.http.Timeout = cloudTimeout
		var st cloudlink.Status
		if off {
			err = c.send(ctx, http.MethodDelete, "/api/v1/settings/meko-cloud", nil, &st)
		} else {
			err = c.send(ctx, http.MethodPut, "/api/v1/settings/meko-cloud", u, &st)
		}
		return st, err
	}
	svc := &cloudlink.Service{Store: store, Check: cloudlink.CheckMCP, CheckAPI: cloudlink.CheckAPI}
	if off {
		return svc.Unlink(ctx)
	}
	st, err := svc.Link(ctx, u)
	var refused *cloudlink.Error
	if errors.As(err, &refused) {
		return st, errors.New(strings.TrimSuffix(refused.Error(), "."))
	}
	return st, err
}

// cloudLine names a linked link's server and key hint.
func cloudLine(st cloudlink.Status) string {
	line := st.EffectiveURL
	if st.KeyHint != nil && *st.KeyHint != "" {
		line += " (key …" + *st.KeyHint + ")"
	}
	return line
}

// cloudStatusLine is meko status's line on the link.
func cloudStatusLine(l paths.Layout) string {
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return "can't read it: " + err.Error()
	}
	store, err := cloudStore(l, cfg)
	if err != nil {
		return "can't read it: " + err.Error()
	}
	st, err := store.Load()
	if err != nil {
		return "can't read it: " + err.Error()
	}
	if !st.Linked {
		return "not linked; extensions use this Meko (meko cloud_link links one)"
	}
	return "linked to " + cloudLine(cloudlink.StatusOf(st))
}

// cloudToken is meko token --json's cloud: the link the extensions use, with its key. set says
// whether the link was ever set: until it is, an extension may hand Meko a key of its own (AgentK
// does, once, with meko cloud_link). Unlinked, its URLs and key are empty.
type cloudToken struct {
	Set    bool   `json:"set"`
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
	WebURL string `json:"web_url"`
	APIURL string `json:"api_url"`
}

func cloudTokenOf(st cloudlink.State) cloudToken {
	if !st.Linked {
		return cloudToken{Set: st.Set}
	}
	l := st.Link
	return cloudToken{Set: true, URL: l.MCPURL(), APIKey: l.Key, WebURL: l.EffectiveWebURL(), APIURL: l.EffectiveAPIURL()}
}

// readSecret reads one line: typed at prompt with echo off on a terminal, else from the input as
// it is (a pipe: echo "$KEY" | meko cloud_link).
func readSecret(in io.Reader, prompt io.Writer, text string) (string, error) {
	f, ok := in.(*os.File)
	if !ok || !interactiveFile(f) {
		line, err := newAnswerReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return "", err
	}
	quiet := *old
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON | unix.ISIG
	if err = unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return "", err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }()
	fmt.Fprint(prompt, text)
	line, err := newAnswerReader(in).ReadString('\n')
	fmt.Fprintln(prompt) // the typed newline wasn't echoed
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func interactiveFile(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// send is do with a JSON body.
func (c *client) send(ctx context.Context, method, path string, body, data any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	// G704: the URL is always 127.0.0.1 on the port serve wrote to the data folder.
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r) //nolint:gosec
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req) //nolint:gosec // G704: see above
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	env := envelope.Response{Data: data}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("unexpected reply from %s (%s): %w", path, resp.Status, err)
	}
	if env.Error != nil {
		return errors.New(strings.TrimSuffix(env.Error.Message, "."))
	}
	return nil
}
