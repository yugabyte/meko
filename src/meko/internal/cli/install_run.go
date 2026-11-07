package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/extensions"
	"github.com/yugabyte/meko/src/meko/internal/paths"
	"github.com/yugabyte/meko/src/meko/internal/service"
	"github.com/yugabyte/meko/src/meko/internal/spin"
	"golang.org/x/sys/unix"
)

// What install.sh tells the meko install it runs, which prints the rest of the same run
// (docs/install.md, "The installer").
const (
	envInstallStarted = "MEKO_INSTALL_STARTED" // when the installer began, in seconds since 1970
	envInstallFrom    = "MEKO_INSTALL_FROM"    // the version it replaces, when it upgrades
	envInstallFirst   = "MEKO_INSTALL_FIRST"   // 1 when no Meko was installed before
	envInstallAsk     = "MEKO_INSTALL_ASK"     // 0 with --non-interactive: ask nothing
)

// installRunInfo is what install.sh told meko install: when the run began (for "installed in 14s"),
// the version it replaces, whether this is the first install, which ends with the extension picker,
// and whether it may ask (the PATH question and the picker). meko install run by hand has none of
// it, and may ask.
type installRunInfo struct {
	started time.Time
	from    string
	first   bool
	ask     bool
}

func installRun(getenv func(string) string) installRunInfo {
	var run installRunInfo
	if secs, err := strconv.ParseInt(getenv(envInstallStarted), 10, 64); err == nil && secs > 0 {
		run.started = time.Unix(secs, 0)
	}
	run.from = getenv(envInstallFrom)
	run.first = getenv(envInstallFirst) == "1"
	run.ask = getenv(envInstallAsk) != "0"
	return run
}

// homeRel is path with the home folder written as ~: "~/.meko".
func homeRel(path, home string) string {
	if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok && home != "" {
		return "~/" + rest
	}
	return path
}

// openTTY opens the terminal for install's questions: the script is piped to sh, so meko install's
// stdin is that pipe, not the keyboard. Tests replace it.
var openTTY = func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }

// askTTY is the terminal install asks its questions on, when its output is a terminal too, or nil.
func askTTY(stdout io.Writer) *os.File {
	if !spin.IsTerminal(stdout) {
		return nil
	}
	f, err := openTTY()
	if err != nil {
		return nil
	}
	if !interactiveFile(f) {
		_ = f.Close()
		return nil
	}
	discardInput(f)
	return f
}

// discardInput drops what the terminal sent that nobody read, so a question doesn't take it as its
// answer: such as the terminal's late reply to the background query (spin.Background, or
// install.sh's), which would make Enter at "Add it? [Y/n]" a no.
func discardInput(f *os.File) {
	if t, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios); err == nil {
		_ = unix.IoctlSetTermios(int(f.Fd()), ioctlFlushTermios, t)
	}
}

// pathMark is the comment before the line meko install adds to a shell's profile, which meko
// uninstall looks for to remove both.
const pathMark = "# Added by meko install (meko uninstall removes it): Meko's commands on the PATH"

// pathProfiles are the shell profiles meko install may add its line to, and meko uninstall looks in.
var pathProfiles = []string{".zshrc", ".bashrc", ".bash_profile"}

// pathProfile is the profile of the shell in $SHELL that meko install adds bin to the PATH in: zsh's
// .zshrc; bash's .bashrc, or .bash_profile on macOS, whose Terminal opens login shells. It's "" for
// other shells, which are told the line to add.
func pathProfile(home string) string {
	switch filepath.Base(os.Getenv("SHELL")) {
	case "zsh":
		return filepath.Join(home, ".zshrc")
	case "bash":
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile")
		}
		return filepath.Join(home, ".bashrc")
	}
	return ""
}

// pathLine is the line that puts bin on the PATH, with the home folder as $HOME.
func pathLine(bin, home string) string {
	if rest, ok := strings.CutPrefix(bin, home+string(filepath.Separator)); ok {
		bin = "$HOME/" + rest
	}
	return `export PATH="` + bin + `:$PATH"`
}

// installRow is install's row for the meko command, linked in bin: on the PATH, or not, when it asks
// (on a terminal, for zsh and bash, when it may ask) to add bin to the shell's profile, or says the
// line to add.
func installRow(r report, bin, home string, ask bool) {
	what := "meko in " + homeRel(bin, home)
	if onPath(bin) {
		r.step(stepOK, "Install", spin.Note(what, "on your PATH", r.color))
		return
	}
	line := pathLine(bin, home)
	profile := pathProfile(home)
	var tty *os.File
	if profile != "" && ask {
		tty = askTTY(r.w)
	}
	if tty == nil {
		r.step(stepWarn, "Install", what+spin.Dim(" · not on your PATH: add ", r.color)+spin.Blue(line, r.color)+
			spin.Dim(" to your shell's profile", r.color))
		return
	}
	defer func() { _ = tty.Close() }()
	r.step(stepWarn, "Install", spin.Note(what, "not on your PATH", r.color))
	fmt.Fprintf(r.w, "  %s %s %s ", spin.Blue("?", r.color),
		spin.Bold(fmt.Sprintf("Add %s to your PATH in %s?", homeRel(bin, home), homeRel(profile, home)), r.color),
		spin.Dim("[Y/n]", r.color))
	answer, err := newAnswerReader(tty).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		answer = "n"
	}
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
		fmt.Fprintf(r.w, "    Add %s to your shell's profile to run meko by its name\n", spin.Blue(line, r.color))
		return
	}
	if err := addPathLine(profile, line); err != nil {
		r.step(stepWarn, "PATH", fmt.Sprintf("can't add it to %s: %v", homeRel(profile, home), err))
		return
	}
	r.step(stepOK, "PATH", "added to "+homeRel(profile, home)+spin.Dim(" · open a new terminal, or run ", r.color)+
		spin.Blue(". "+homeRel(profile, home), r.color))
}

// addPathLine adds line to the profile, after pathMark, unless the profile has them already.
func addPathLine(profile, line string) error {
	b, err := os.ReadFile(profile) //nolint:gosec // G304: the user's own shell profile
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains(string(b), pathMark+"\n") {
		return nil
	}
	f, err := os.OpenFile(profile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644) //nolint:gosec // G302,G304: a shell profile
	if err != nil {
		return err
	}
	add := "\n" + pathMark + "\n" + line + "\n"
	if len(b) > 0 && b[len(b)-1] != '\n' {
		add = "\n" + add
	}
	if _, err = f.WriteString(add); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// removePathLines removes the lines meko install added to the shell profiles in home (pathMark, the
// line after it, and the blank line it put before them). It returns the profiles it changed.
func removePathLines(home string) ([]string, error) {
	var changed []string
	for _, name := range pathProfiles {
		p := filepath.Join(home, name)
		b, err := os.ReadFile(p) //nolint:gosec // G304: the user's own shell profile
		if errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return changed, err
		}
		lines := strings.SplitAfter(string(b), "\n")
		var kept []string
		found := false
		for i := 0; i < len(lines); i++ {
			if strings.TrimSuffix(lines[i], "\n") != pathMark {
				kept = append(kept, lines[i])
				continue
			}
			found = true
			if n := len(kept); n > 0 && kept[n-1] == "\n" {
				kept = kept[:n-1]
			}
			i++ // the export line
		}
		if !found {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			return changed, err
		}
		//nolint:gosec // G703: one of pathProfiles in the user's own home folder
		if err = os.WriteFile(p, []byte(strings.Join(kept, "")), fi.Mode().Perm()); err != nil {
			return changed, err
		}
		changed = append(changed, p)
	}
	return changed, nil
}

// mekoHolds reports whether a Meko server that install replaces listens on port: this data
// folder's, or the one the installed service runs. Its port file names the port agents use; a Meko
// from before one port listened on its web UI's too (7777, beside 7778 in the port file), which
// its status reports.
func mekoHolds(l paths.Layout, m service.Manager, port int) bool {
	dirs := []string{l.Root}
	if m != nil && m.Installed() && m.DataDir() != "" {
		dirs = append(dirs, m.DataDir())
	}
	for _, d := range dirs {
		dl := paths.New(d)
		if up, _ := running(dl.Lock); !up {
			continue
		}
		if p, err := readInt(dl.Port); err == nil && p == port {
			return true
		}
		if webPort(dl) == port {
			return true
		}
	}
	return false
}

// webPort is the port a running server's status says its web UI is on, or 0.
func webPort(l paths.Layout) int {
	c, err := newClient(l)
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st, err := c.status(ctx)
	if err != nil || st.Web == nil {
		return 0
	}
	u, err := url.Parse(st.Web.URL)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(u.Port())
	return p
}

// portFree reports, as an error, another program listening on port on this computer; tests replace it.
var portFree = func(port int) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return ln.Close()
}

// portInUse prints install's failed Server row for a port other programs hold, with how to find
// the one on port, and how to move Meko.
func portInUse(r report, msg string, port int, move string) {
	r.step(stepFail, "Server", msg)
	find := fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", port)
	if runtime.GOOS == "linux" {
		find = fmt.Sprintf("ss -ltnp 'sport = :%d'", port)
	}
	fmt.Fprintf(r.w, "\n    %s%s\n", spin.Dim("See what holds it    ", r.color), spin.Blue(find, r.color))
	fmt.Fprintf(r.w, "    %s%s\n", spin.Dim("Or move Meko         ", r.color), move)
}

// addLater is what a first install says in place of the picker, where it can't ask.
const addLater = "Add an extension at any time: meko extension install kode"

// upcoming are the experiences a first install's picker lists as coming soon. They aren't
// extensions yet (nothing in extensions.Known), so they can't be picked or installed.
var upcoming = []pickRow{
	{label: "migrate", detail: "database migrations with a safety net", soon: true},
	{label: "scribe", detail: "briefs, decks, docs and posts from your context", soon: true},
}

// skipRow is the picker's last row, which adds none.
var skipRow = pickRow{label: "skip for now", detail: "explore the base platform — add an experience anytime"}

// pickExtension is a first install's last question, asked after the links: "Choose your agentic
// experience", which of the known extensions not installed yet that have a Summary to add (each by
// its Verb, the meko command that runs it, with its Summary), or none; the upcoming ones are listed
// too, dim, and can't be picked. The one picked is installed as meko extension install does, on the terminal,
// where it asks its own questions; it returns its name, or "" for none. Where it can't (no
// terminal) or mayn't (--non-interactive) ask, it says how to add one later. A failed extension
// install is a ✗ row and its error, and Meko stays installed.
func pickExtension(l paths.Layout, ask bool, stdout, stderr io.Writer) func(r report) string {
	return func(r report) string {
		reg, err := extensions.Load(l.Root)
		if err != nil {
			return ""
		}
		var names []string
		var rows []pickRow
		for _, n := range extensions.KnownNames() {
			c := extensions.Known[n]
			if _, ok := reg.Entries[n]; ok || c.Summary == "" {
				continue
			}
			names = append(names, n)
			rows = append(rows, pickRow{label: cmp.Or(c.Verb, n), detail: c.Summary})
		}
		if len(names) == 0 {
			return ""
		}
		// Kode first: it's what most people add.
		for i, n := range names {
			if n == "kode" && i > 0 {
				names[0], names[i] = names[i], names[0]
				rows[0], rows[i] = rows[i], rows[0]
			}
		}
		var tty *os.File
		if ask {
			tty = askTTY(stdout)
		}
		if tty == nil {
			fmt.Fprintf(stdout, "\n  %s\n", addLater)
			return ""
		}
		defer func() { _ = tty.Close() }()
		fmt.Fprintln(stdout)
		rows = append(append(rows, upcoming...), skipRow)
		i, err := pickRows(tty, stdout, "Choose your agentic experience", rows, false)
		if errors.Is(err, errNoKeyMode) {
			fmt.Fprintf(stdout, "  %s\n", addLater)
			return ""
		}
		if err != nil || i >= len(names) {
			return ""
		}
		name := names[i]
		fmt.Fprintf(stdout, "\n  %s %s%s\n", spin.Blue("→", r.color), spin.Blue("meko extension install "+name, r.color),
			spin.Dim("   · adds an extension at any time", r.color))
		err = installExtension(context.Background(), l, name, extInstallOptions{}, true, tty, stdout, stderr)
		if err != nil {
			r.step(stepFail, rows[i].label, "not added: "+err.Error())
			return ""
		}
		return name
	}
}
