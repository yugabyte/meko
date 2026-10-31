package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/yugabyte/meko/backend/internal/paths"
)

// atPrompt is set while meko-cli's prompt runs: add_memory then needs its text on the line, since
// stdin is where the next commands come from.
var atPrompt bool

// datapackCommands are meko-cli's commands that take --datapack, which the prompt's use fills in.
var datapackCommands = []string{"search", "add_memory", "upload", "upload_status"}

// RunMekoCLI runs meko-cli: the command in args, or with none (only --data-dir) its prompt.
func RunMekoCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || args[0] == "-h" || args[0] == "--help") {
		return runProg(progMekoCLI, args, stdout, stderr)
	}
	fs := flag.NewFlagSet(progMekoCLI, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, progMekoCLI, false)
			return exitOK
		}
		fmt.Fprintln(stderr, "Run meko-cli --help to see the commands")
		return exitUsage
	}
	if fs.NArg() > 0 {
		rest := fs.Args()
		if *dataDir != "" {
			rest = append(rest, "--data-dir", *dataDir)
		}
		return runProg(progMekoCLI, rest, stdout, stderr)
	}
	return runPrompt(*dataDir, stdin, stdout, stderr)
}

// runPrompt reads meko-cli's commands from in, one a line, and runs each as meko-cli <line> would, until
// exit, quit or the end of input. On a terminal it prints a banner and a prompt, and a failed line
// doesn't change the exit code; from a pipe or file it's a script, and exits with the first failing
// line's code.
func runPrompt(dataDir string, in io.Reader, stdout, stderr io.Writer) int {
	root, err := paths.Resolve(dataDir)
	if err != nil {
		return fail(stderr, err)
	}
	l := paths.New(root)
	if up, err := running(l.Lock); err != nil {
		return fail(stderr, err)
	} else if !up {
		newReport(stderr).headline(stepFail, fmt.Sprintf("Meko isn't running (data in %s). Start it: meko start", l.Root))
		return exitNotRunning
	}
	interactive := isTerminal(in)
	a := newAnswerReader(in)
	// delete_datapack's question reads the next line, from the same buffer.
	savedStdin := stdin
	stdin, atPrompt = a, true
	defer func() { stdin, atPrompt = savedStdin, false }()
	if interactive {
		c, err := newClient(l)
		if err != nil {
			return fail(stderr, fmt.Errorf("the server is running, but %w", err))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		st, err := c.status(ctx)
		cancel()
		if err != nil {
			return fail(stderr, fmt.Errorf("the server is running, but isn't answering: %w", err))
		}
		fmt.Fprintf(stdout, "Connected to Meko %s at http://127.0.0.1:%d, datapack default\n", st.Version, st.Port)
		fmt.Fprintln(stdout, "Type help for the commands, exit to leave")
	}
	// The datapack the prompt's use picked, by its id, which later lines pass (it still names it after
	// a rename), and its name, which the prompt shows. The zero value is the default.
	var datapack target
	code := exitOK
	for {
		if interactive {
			label := datapack.name
			if label == "" {
				label = "default"
			}
			fmt.Fprintf(stdout, "meko-cli %s> ", label)
		}
		line, err := a.ReadString('\n')
		if line == "" && err != nil {
			if interactive {
				fmt.Fprintln(stdout)
			}
			return code
		}
		words, werr := splitWords(line)
		lc := exitOK
		switch {
		case werr != nil:
			fmt.Fprintf(stderr, "meko-cli: %v\n", werr)
			lc = exitUsage
		case len(words) == 0:
		case words[0] == "exit" || words[0] == "quit":
			return code
		case words[0] == "use":
			lc = promptUse(l, words[1:], &datapack, stdout, stderr)
		default:
			// The datapack a delete_datapack line deletes, found as the command finds it, to tell
			// whether it's the one in use.
			var deleting target
			var found bool
			if words[0] == "delete_datapack" && datapack.id != "" {
				deleting, found = promptResolve(l, firstArg(words[1:]))
			}
			lc = runProg(progMekoCLI, promptArgs(words, datapack.id, dataDir), stdout, stderr)
			// The datapack in use is gone: later lines go to the default one again.
			if lc == exitOK && found && strings.EqualFold(deleting.id, datapack.id) {
				datapack = target{}
				fmt.Fprintln(stdout, "Using the default datapack")
			}
		}
		if lc != exitOK && code == exitOK && !interactive {
			code = lc
		}
	}
}

// promptArgs adds what the prompt knows to a line's words: the datapack use set, for a command that
// takes --datapack and names none, and the data folder meko-cli was given. help takes neither: its
// argument is the command to explain.
func promptArgs(words []string, datapack, dataDir string) []string {
	if words[0] == "help" || words[0] == "-h" || words[0] == "--help" {
		return words
	}
	// has reports whether the line passes the flag: only words that start with - can, so a search
	// for "datapack" doesn't count.
	has := func(name string) bool {
		return slices.ContainsFunc(words[1:], func(w string) bool {
			if !strings.HasPrefix(w, "-") {
				return false
			}
			w = strings.TrimLeft(w, "-")
			return w == name || strings.HasPrefix(w, name+"=")
		})
	}
	out := slices.Clone(words)
	if datapack != "" && slices.Contains(datapackCommands, words[0]) && !has("datapack") {
		out = append(out, "--datapack", datapack)
	}
	if dataDir != "" && !has("data-dir") {
		out = append(out, "--data-dir", dataDir)
	}
	return out
}

// firstArg is the first of args that isn't a flag: delete_datapack's name or id.
func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// promptUse is the prompt's use: with a name or id, find the datapack and use it from now on; alone,
// say which one is in use.
func promptUse(l paths.Layout, args []string, datapack *target, stdout, stderr io.Writer) int {
	switch len(args) {
	case 0:
		if datapack.id == "" {
			fmt.Fprintln(stdout, "Using the default datapack")
		} else {
			fmt.Fprintf(stdout, "Using %s\n", datapack.noun())
		}
		return exitOK
	case 1:
	default:
		fmt.Fprintln(stderr, "meko-cli: use takes one datapack, by name or id")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	session, err := connectTools(ctx, l)
	if err != nil {
		fmt.Fprintf(stderr, "meko-cli: the server is running, but %v\n", err)
		return exitError
	}
	t := newTools(session, l)
	defer t.close()
	dp, err := t.resolve(ctx, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "meko-cli: %v\n", err)
		return exitError
	}
	*datapack = dp
	fmt.Fprintf(stdout, "Using %s\n", dp.noun())
	return exitOK
}

// promptResolve finds the datapack a line names as its command will, and whether it was found.
func promptResolve(l paths.Layout, named string) (target, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	session, err := connectTools(ctx, l)
	if err != nil {
		return target{}, false
	}
	t := newTools(session, l)
	defer t.close()
	dp, err := t.resolve(ctx, named)
	return dp, err == nil && dp.id != ""
}

// splitWords splits a prompt line into words as a shell would: blanks separate them, single quotes
// keep everything, double quotes keep everything but a backslash before " \ $ or `, a backslash
// outside quotes keeps the next character, and # starts a comment.
func splitWords(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord, quote := false, rune(0)
	rs := []rune(strings.TrimRight(line, "\r\n"))
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[i+1]):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == '\\':
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
				inWord = true
			}
		case r == '#' && !inWord:
			return words, nil
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("the line has an unclosed %c quote", quote)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
