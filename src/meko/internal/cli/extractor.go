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
	"time"

	"github.com/yugabyte/meko/src/meko/internal/memory"
	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// extractorTimeout bounds a call that runs the harness: use's test call, and a run.
const extractorTimeout = 10 * time.Minute

// extractorStatus is GET /settings/memory-extractor.
type extractorStatus struct {
	Enabled         bool   `json:"enabled"`
	Harness         string `json:"harness"`
	Command         string `json:"command"`
	Model           string `json:"model"`
	IntervalSeconds int    `json:"interval_seconds"`
	Queued          int    `json:"queued"`
	LastRunAt       string `json:"last_run_at"`
	LastError       string `json:"last_error"`
	Extracted       int    `json:"extracted"`
}

// datapackExtraction is GET /datapacks/:id/memory-extraction.
type datapackExtraction struct {
	OptOut       bool   `json:"opt_out"`
	Profile      string `json:"profile"`
	Instructions string `json:"instructions"`
}

// runMemoryExtractor is meko memory-extractor use|off|status|run|datapack.
func runMemoryExtractor(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		c, _ := lookup(progMeko, "memory-extractor")
		printCommandHelp(stdout, progMeko, c, nil)
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("meko memory-extractor "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	every := fs.Duration("every", 0, "how often it runs (default 5m, at least 1m)")
	model := fs.String("model", "", "the model the harness uses (default haiku)")
	asJSON := fs.Bool("json", false, "print raw JSON")
	profile := fs.String("profile", "", "the datapack's profile: "+strings.Join(memory.ExtractionProfiles(), " or "))
	instructions := fs.String("instructions", "", "the datapack's own instructions (\"\" clears them)")
	off := fs.Bool("off", false, "don't extract this datapack's conversations")
	on := fs.Bool("on", false, "extract this datapack's conversations again")
	// The one argument (the harness or the datapack) may come before the flags or after them.
	var arg string
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		arg, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); errors.Is(err, flag.ErrHelp) {
		c, _ := lookup(progMeko, "memory-extractor")
		printCommandHelp(stdout, progMeko, c, fs)
		return exitOK
	} else if err != nil {
		fmt.Fprintln(stderr, "See meko memory-extractor --help")
		return exitUsage
	}
	if arg == "" && fs.NArg() > 0 {
		arg = fs.Arg(0)
	} else if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "meko memory-extractor %s: unexpected argument %q\n", sub, fs.Arg(0))
		return exitUsage
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
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
		err = extractorUse(ctx, c, arg, *model, *every, stdout)
	case "off":
		err = c.send(ctx, http.MethodPut, "/api/v1/settings/memory-extractor", map[string]string{"harness": ""}, nil)
		if err == nil {
			fmt.Fprintln(stdout, "Memory extraction is off: turns saved from now on aren't queued")
		}
	case "status":
		err = extractorPrintStatus(ctx, c, *asJSON, stdout)
	case "run":
		var run memory.ExtractRun
		if err = c.send(ctx, http.MethodPost, "/api/v1/settings/memory-extractor/run", nil, &run); err == nil {
			fmt.Fprintf(stdout, "Extracted %d memories from %d turns in %d conversations\n", run.Memories, run.Turns,
				run.Conversations)
			if run.Failed > 0 {
				fmt.Fprintf(stdout, "%d conversations failed and stay queued: %s\n", run.Failed, run.LastError)
			}
		}
	case "datapack":
		if *off && *on {
			fmt.Fprintln(stderr, "meko memory-extractor datapack: --off or --on, not both")
			return exitUsage
		}
		err = extractorDatapack(ctx, c, arg, set, *profile, *instructions, *off, *on, stdout)
	default:
		fmt.Fprintf(stderr, "meko memory-extractor: unknown subcommand %q: use, off, status, run or datapack\n", sub)
		return exitUsage
	}
	if err != nil {
		return fail(stderr, err)
	}
	return exitOK
}

// extractorUse finds the harness's program on this PATH (the service's own may not have it) and
// turns extraction on with it; the server checks one call works first.
func extractorUse(ctx context.Context, c *client, harness, model string, every time.Duration, stdout io.Writer) error {
	if harness == "" {
		return errors.New("name the harness: meko memory-extractor use claude")
	}
	command, err := exec.LookPath(harness)
	if err != nil {
		return fmt.Errorf("can't find %s on your PATH: install it and sign in first", harness)
	}
	fmt.Fprintf(stdout, "Checking that %s answers with one short call\n", command)
	var st extractorStatus
	if err = c.send(ctx, http.MethodPut, "/api/v1/settings/memory-extractor", map[string]any{"harness": harness,
		"command": command, "model": model, "interval_seconds": int(every / time.Second)}, &st); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Memory extraction is on: %s (%s), every %s. Turns saved from now on are extracted; "+
		"meko memory-extractor run extracts now\n", st.Harness, st.Model, time.Duration(st.IntervalSeconds)*time.Second)
	return nil
}

func extractorPrintStatus(ctx context.Context, c *client, asJSON bool, stdout io.Writer) error {
	var st extractorStatus
	if err := c.get(ctx, "/api/v1/settings/memory-extractor", &st); err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	if !st.Enabled {
		fmt.Fprintln(stdout, "Memory extraction is off. Turn it on with: meko memory-extractor use claude")
		return nil
	}
	fmt.Fprintf(stdout, "Memory extraction: on, %s (%s) every %s\n", st.Harness, st.Model,
		time.Duration(st.IntervalSeconds)*time.Second)
	fmt.Fprintf(stdout, "  Command:   %s\n  Waiting:   %d turns\n  Extracted: %d memories\n", st.Command, st.Queued,
		st.Extracted)
	if st.LastRunAt != "" {
		fmt.Fprintf(stdout, "  Last run:  %s\n", st.LastRunAt)
	}
	if st.LastError != "" {
		fmt.Fprintf(stdout, "  Last error: %s\n", st.LastError)
	}
	return nil
}

// extractorDatapack shows a datapack's extraction settings, or changes the ones its flags set.
func extractorDatapack(ctx context.Context, c *client, name string, set map[string]bool, profile, instructions string,
	off, on bool, stdout io.Writer) error {
	if name == "" {
		return errors.New("name the datapack: meko memory-extractor datapack <name|id> [--profile coding]")
	}
	var dps []struct {
		ID   string `json:"datapack_id"`
		Name string `json:"datapack_name"`
	}
	if err := c.get(ctx, "/api/v1/datapacks", &dps); err != nil {
		return err
	}
	id := ""
	for _, d := range dps {
		if d.ID == name || strings.EqualFold(d.Name, name) {
			id = d.ID
		}
	}
	if id == "" {
		return fmt.Errorf("no datapack %q (meko datapacks lists them)", name)
	}
	change := map[string]any{}
	if set["profile"] {
		change["profile"] = profile
	}
	if set["instructions"] {
		change["instructions"] = instructions
	}
	if off || on {
		change["opt_out"] = off
	}
	var d datapackExtraction
	path := "/api/v1/datapacks/" + id + "/memory-extraction"
	var err error
	if len(change) > 0 {
		err = c.send(ctx, http.MethodPatch, path, change, &d)
	} else {
		err = c.get(ctx, path, &d)
	}
	if err != nil {
		return err
	}
	state := "extracted"
	if d.OptOut {
		state = "not extracted (opted out)"
	}
	fmt.Fprintf(stdout, "%s: conversations %s, profile %s\n", name, state, d.Profile)
	if d.Instructions != "" {
		fmt.Fprintf(stdout, "  Instructions: %s\n", d.Instructions)
	}
	return nil
}
