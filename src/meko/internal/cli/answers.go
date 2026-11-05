package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// answersStatus is GET /settings/answers.
type answersStatus struct {
	Enabled bool   `json:"enabled"`
	Harness string `json:"harness"`
	Command string `json:"command"`
	Model   string `json:"model"`
}

// runAnswers is meko answers use|off|status.
func runAnswers(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		c, _ := lookup(progMeko, "answers")
		printCommandHelp(stdout, progMeko, c, nil)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("meko answers "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	model := fs.String("model", "", "the model the harness uses (default haiku)")
	asJSON := fs.Bool("json", false, "print raw JSON")
	// The harness may come before the flags or after them.
	var arg string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		arg, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); errors.Is(err, flag.ErrHelp) {
		c, _ := lookup(progMeko, "answers")
		printCommandHelp(stdout, progMeko, c, fs)
		return exitOK
	} else if err != nil {
		fmt.Fprintln(stderr, "See meko answers --help")
		return exitUsage
	}
	if arg == "" && fs.NArg() > 0 {
		arg = fs.Arg(0)
	} else if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "meko answers %s: unexpected argument %q\n", sub, fs.Arg(0))
		return exitUsage
	}
	root, err := paths.Resolve(*dataDir)
	if err != nil {
		return fail(stderr, err)
	}
	c, err := newClient(paths.New(root))
	if err != nil {
		return fail(stderr, fmt.Errorf("%w (is Meko running? meko start)", err))
	}
	c.http.Timeout = extractorTimeout
	ctx := context.Background()
	switch sub {
	case "use":
		err = answersUse(ctx, c, arg, *model, stdout)
	case "off":
		err = c.send(ctx, http.MethodPut, "/api/v1/settings/answers", map[string]string{"harness": ""}, nil)
		if err == nil {
			fmt.Fprintln(stdout, "Search answers are off: Search shows its results only")
		}
	case "status":
		err = answersPrintStatus(ctx, c, *asJSON, stdout)
	default:
		fmt.Fprintf(stderr, "meko answers: unknown subcommand %q: use, off or status\n", sub)
		return exitUsage
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// answersUse finds the harness's program on this PATH (the service's own may not have it) and turns
// answers on with it; the server checks one call works first.
func answersUse(ctx context.Context, c *client, harness, model string, stdout io.Writer) error {
	if harness == "" {
		return errors.New("name the harness: meko answers use claude")
	}
	command, err := exec.LookPath(harness)
	if err != nil {
		return fmt.Errorf("can't find %s on your PATH: install it and sign in first", harness)
	}
	fmt.Fprintf(stdout, "Checking that %s answers with one short call\n", command)
	var st answersStatus
	if err = c.send(ctx, http.MethodPut, "/api/v1/settings/answers", map[string]any{"harness": harness,
		"command": command, "model": model}, &st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Search answers are on: %s (%s) writes an answer from each search's results in the web UI\n",
		st.Harness, st.Model)
	return nil
}

func answersPrintStatus(ctx context.Context, c *client, asJSON bool, stdout io.Writer) error {
	var st answersStatus
	if err := c.get(ctx, "/api/v1/settings/answers", &st); err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	if !st.Enabled {
		fmt.Fprintln(stdout, "Search answers are off. Turn them on with: meko answers use claude")
		return nil
	}
	fmt.Fprintf(stdout, "Search answers: on, %s (%s)\n  Command: %s\n", st.Harness, st.Model, st.Command)
	return nil
}
