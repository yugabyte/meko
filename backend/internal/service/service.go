// Package service runs meko serve as a login service: a launchd LaunchAgent on macOS, a systemd
// user unit on Linux. It writes the service file from an embedded template, and starts, stops and
// restarts the service through launchctl or systemctl. The CLI uses a service only when its file
// runs the data folder in question (DataDir), so a meko run on another data folder, such as a
// test's, never touches it.
package service

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"text/template"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// Names of the service.
const (
	Label = "com.yugabyte.meko" // the LaunchAgent
	Unit  = "meko.service"      // the systemd user unit
)

// Spec is what the service runs.
type Spec struct {
	Exe        string // the meko binary: app/current/bin/meko once installed from a release
	DataDir    string // passed as --data-dir, so the service and the CLI agree on it
	ServiceLog string // the service's stderr: errors from before serve's own log opens
	Host       string // systemd: the only host the unit runs on
}

// Runner runs a command and returns its trimmed stdout; the error carries its stderr.
type Runner func(name string, args ...string) (string, error)

// Exec is the Runner that runs real commands.
func Exec(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: launchctl, systemctl, loginctl
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(out.String()), nil
}

// Manager is the service manager of this OS.
type Manager interface {
	Kind() string // "launchd" or "systemd"
	// File is where the service file lives.
	File() string
	// Installed reports whether the service file exists.
	Installed() bool
	// DataDir is the data folder the installed service runs ("" if none is installed).
	DataDir() string
	// Preflight says, as an error, why this machine can't run the service.
	Preflight() error
	// Install writes the service file, enables the service and (re)starts it.
	Install(Spec) error
	// Start starts the service; a running one is left alone.
	Start() error
	// Stop stops the service until Start, the next login or Install.
	Stop() error
	// Restart restarts it.
	Restart() error
	// Running reports whether the service runs.
	Running() bool
	// Remove stops the service and deletes its file.
	Remove() error
	// StatusHint is the command that shows the service manager's view of it.
	StatusHint() string
}

// ErrUnsupported means the OS has no service manager Meko supports.
var ErrUnsupported = errors.New("a Meko service runs only on macOS (launchd) and Linux (systemd)")

// For returns the service manager of this OS for the user whose home is home.
func For(home string, run Runner) (Manager, error) {
	switch runtime.GOOS {
	case "darwin":
		return &Launchd{Home: home, UID: os.Getuid(), Run: run}, nil
	case "linux":
		return &Systemd{Home: home, ConfigHome: os.Getenv("XDG_CONFIG_HOME"), User: os.Getenv("USER"), Run: run}, nil
	}
	return nil, ErrUnsupported
}

//go:embed meko.plist.tmpl
var plistTmpl string

//go:embed meko.service.tmpl
var unitTmpl string

var tmplFuncs = template.FuncMap{
	"xml": func(s string) (string, error) {
		var b strings.Builder
		err := xml.EscapeText(&b, []byte(s))
		return b.String(), err
	},
	// arg quotes a word of ExecStart: systemd reads C-style quotes, % specifiers and $ variables.
	"arg": func(s string) string {
		r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`, `$`, `$$`)
		return `"` + r.Replace(s) + `"`
	},
	// path is a file path where systemd takes no quotes (StandardError=append:), only specifiers.
	"path": func(s string) string { return strings.ReplaceAll(s, "%", "%%") },
}

// render fills the template with the spec.
func render(tmpl string, s Spec) ([]byte, error) {
	t, err := template.New("service").Funcs(tmplFuncs).Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	err = t.Execute(&b, struct {
		Spec
		Label    string
		Launcher string
	}{s, Label, Launcher(s.DataDir)})
	return b.Bytes(), err
}

// writeFile writes a service file, creating its folder (0755: launchd and systemd only read it).
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: a service folder
		return err
	}
	return paths.WriteFile(path, data)
}

var plistDataDir = regexp.MustCompile(`--data-dir</string>\s*<string>([^<]*)</string>`)

// dataDirOf reads the --data-dir a service file passes ("" if none).
func dataDirOf(file string) string {
	b, err := os.ReadFile(file) //nolint:gosec // G304: our own service file
	if err != nil {
		return ""
	}
	if strings.HasSuffix(file, ".plist") {
		m := plistDataDir.FindSubmatch(b)
		if m == nil {
			return ""
		}
		return html.UnescapeString(string(m[1]))
	}
	// The unit's ExecStart quotes it as the arg template function does.
	_, rest, ok := strings.Cut(string(b), `--data-dir "`)
	if !ok {
		return ""
	}
	var out strings.Builder
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		switch {
		case c == '"':
			return out.String()
		case c == '\\' && i+1 < len(rest):
			i++
			out.WriteByte(rest[i])
		case (c == '%' || c == '$') && i+1 < len(rest) && rest[i+1] == c:
			i++
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return ""
}
