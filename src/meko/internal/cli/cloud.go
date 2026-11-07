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

	"github.com/yugabyte/meko/src/meko/internal/agents"
	"github.com/yugabyte/meko/src/meko/internal/cloudlink"
	"github.com/yugabyte/meko/src/meko/internal/config"
	"github.com/yugabyte/meko/src/meko/internal/envelope"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/secrets"
)

// cloudFlags are meko cloud-link's.
type cloudFlags struct {
	url, webURL, apiURL string
	off, noBrowser      bool
	set                 map[string]bool // the flags given, so --url "" (Meko Cloud's) differs from no --url
}

// cloudTimeout bounds a link through the server: its two checks (cloudlink.CheckTimeout each) and
// moving the agents (agents.MoveTimeout), with a minute to spare, so it never gives up on a change
// the server goes on to finish.
var cloudTimeout = 2*cloudlink.CheckTimeout + agents.MoveTimeout + time.Minute

func cloudStore(l paths.Layout, cfg config.Config) (cloudlink.Store, error) {
	s, err := secrets.Open(cfg.Secrets.Store, l.Root)
	if err != nil {
		return cloudlink.Store{}, err
	}
	return cloudlink.Store{File: cloudlink.File(l.Config), Secrets: s}, nil
}

// runCloudLink links Meko Cloud, or another Meko server, for the extensions: on a terminal by signing
// in with the browser or with a key typed at a hidden prompt, as the person picks; otherwise with the
// key from stdin. --off unlinks. With a server running it goes through it (the admin
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
	keyName := "" // the key a sign-in made, named in an error if the link then fails
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
		signIn, answers, cerr := chooseSignIn(in, stderr)
		if cerr != nil {
			return fail(stderr, cerr)
		}
		if signIn {
			// The change is pinned to the server signed in to, so the key goes nowhere else.
			pinned, mcpURL, serr := (&cloudlink.Service{Store: store}).SignInTarget(u)
			if serr != nil {
				return fail(stderr, withoutPeriod(serr))
			}
			u = pinned
			minted, serr := signInWithBrowser(mcpURL, f.noBrowser, answers, stderr)
			if serr != nil {
				return fail(stderr, withoutPeriod(serr))
			}
			u.Key, keyName = &minted.Key, minted.Name
		} else {
			prompt := "Meko Cloud API key: "
			if cur.Linked {
				prompt = "Meko Cloud API key (Enter keeps the saved one): "
			}
			key, rerr := readSecret(answers, stderr, prompt)
			if rerr != nil {
				return fail(stderr, rerr)
			}
			u.Key = &key
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), cloudTimeout)
	defer cancel()
	st, err := applyCloud(ctx, l, store, f.off, u)
	if err != nil {
		if keyName != "" {
			err = withoutPeriod(cloudlink.KeyLeft(err, keyName))
		}
		return fail(stderr, err)
	}
	r := newReport(stdout)
	if st.Linked {
		r.headline(stepOK, "Linked to "+cloudLine(st))
	} else {
		fmt.Fprintln(stdout, "Meko Cloud is unlinked: extensions use this Meko")
	}
	for _, line := range moveLines(st.Agents) {
		fmt.Fprintln(stdout, line)
	}
	if st.Linked {
		var c *client
		if up, _ := running(l.Lock); up {
			c, _ = newClient(l)
		}
		r.next(nextSteps(readNextState(l, c), nextUse))
	}
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
	_, a, err := readMCPAccess(l)
	if err != nil {
		return cloudlink.Status{}, err
	}
	svc := &cloudlink.Service{Store: store, Check: cloudlink.CheckMCP, CheckAPI: cloudlink.CheckAPI,
		Moved: cloudMover(l, localTarget(a))}
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

// withoutPeriod is a message from cloudlink as the CLI's last line, which ends without a period.
func withoutPeriod(err error) error { return errors.New(strings.TrimSuffix(err.Error(), ".")) }

// cloudLine names a linked link's server and key hint.
func cloudLine(st cloudlink.Status) string {
	line := st.EffectiveURL
	if st.KeyHint != nil && *st.KeyHint != "" {
		line += " (key …" + *st.KeyHint + ")"
	}
	return line
}

// readCloudLink reads the Meko Cloud link as the data folder at l keeps it.
func readCloudLink(l paths.Layout) (cloudlink.State, error) {
	cfg, err := config.Load(l.Config, os.Getenv, config.Overrides{})
	if err != nil {
		return cloudlink.State{}, err
	}
	store, err := cloudStore(l, cfg)
	if err != nil {
		return cloudlink.State{}, err
	}
	return store.Load()
}

// cloudStatusLine is meko status's line on the link.
func cloudStatusLine(l paths.Layout) string {
	st, err := readCloudLink(l)
	if err != nil {
		return "can't read it: " + err.Error()
	}
	if !st.Linked {
		return "not linked"
	}
	return "linked to " + cloudLine(cloudlink.StatusOf(st))
}

// cloudRow is the summary's Cloud link row: the linked Meko Cloud's web app, or for a link with
// none (another Meko server, with no --web-url) its MCP URL and key hint; or "not linked".
func cloudRow(l paths.Layout) linkRow {
	st, err := readCloudLink(l)
	switch {
	case err != nil:
		return linkRow{label: "Cloud", note: "can't read the link: " + err.Error()}
	case !st.Linked:
		return linkRow{label: "Cloud", note: "not linked"}
	}
	if web := st.Link.EffectiveWebURL(); web != "" {
		return linkRow{label: "Cloud", url: web}
	}
	return linkRow{label: "Cloud", note: cloudLine(cloudlink.StatusOf(st))}
}

// cloudToken is meko token --json's cloud: whether the extensions' Meko is linked to Meko Cloud,
// never its key. Linked, they reach Meko Cloud through this Meko, which holds the key: its /mcp and
// its /api/v1 serve this Meko's datapacks and send the rest to Meko Cloud, and they link to its web
// app at web_url. set says whether the link was ever set. With error, it couldn't be read, and an
// extension keeps what it last read.
type cloudToken struct {
	Set    bool   `json:"set"`
	Linked bool   `json:"linked"`
	WebURL string `json:"web_url"`
	// Account names the linked account without its key (AccountOf), so an extension knows a new key
	// is another account; "" while unlinked.
	Account string `json:"account"`
	// DatapackRouting is always true. Only AgentK and Presenter releases from before every datapack
	// was routed by id read it: without it they call /api/v1/cloud, which this Meko doesn't have.
	// Current releases ignore it; it stays until those older ones are gone.
	DatapackRouting bool `json:"datapack_routing,omitempty"`
	// Error, when set, is why the link couldn't be read; the rest is then empty.
	Error string `json:"error,omitempty"`
}

// cloudTokenFor is the link for token --json; unreadable, its error says why and nothing else.
func cloudTokenFor(l paths.Layout, cfg config.Config) cloudToken {
	store, err := cloudStore(l, cfg)
	if err == nil {
		var st cloudlink.State
		if st, err = store.Load(); err == nil {
			return cloudTokenOf(st)
		}
	}
	return cloudToken{Error: err.Error()}
}

func cloudTokenOf(st cloudlink.State) cloudToken {
	if !st.Linked {
		return cloudToken{Set: st.Set, DatapackRouting: true}
	}
	return cloudToken{Set: true, Linked: true, WebURL: st.Link.EffectiveWebURL(), Account: st.Link.Account(),
		DatapackRouting: true}
}

// readSecret reads one line: typed at prompt with echo off on a terminal, else from the input as
// it is (a pipe: echo "$KEY" | meko cloud-link).
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
		return &apiError{code: env.Error.Code, msg: strings.TrimSuffix(env.Error.Message, ".")}
	}
	return nil
}

// apiError is the running server's error reply: its code, and its message for people.
type apiError struct{ code, msg string }

func (e *apiError) Error() string { return e.msg }
