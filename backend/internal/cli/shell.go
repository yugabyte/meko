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

// atPrompt is set while mekosh's prompt runs: add_memory then needs its text on the line, since
// stdin is where the next commands come from.
var atPrompt bool

// datapackCommands are mekosh's commands that take --datapack, which the prompt's use fills in.
var datapackCommands = []string{"search", "add_memory", "upload", "upload_status"}

// promptDatapack is the datapack the prompt's use picked: its id, which later lines pass (it still
// names it after a rename), and its name, which the prompt shows. The zero value is the default.
type promptDatapack struct{ id, name string }

// RunMekosh runs mekosh: the command in args, or with none (only --data-dir) its prompt.
func RunMekosh(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (!strings.HasPrefix(args[0], "-") || args[0] == "-h" || args[0] == "--help") {
		return runProg(progMekosh, args, stdout, stderr)
	}
	fs := flag.NewFlagSet(progMekosh, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout, progMekosh)
			return exitOK
		}
		fmt.Fprintln(stderr, "Run mekosh --help to see the commands")
		return exitUsage
	}
	if fs.NArg() > 0 {
		rest := fs.Args()
		if *dataDir != "" {
			rest = append(rest, "--data-dir", *dataDir)
		}
		return runProg(progMekosh, rest, stdout, stderr)
	}
	return runPrompt(*dataDir, stdin, stdout, stderr)
}

// runPrompt reads mekosh's commands from in, one a line, and runs each as mekosh <line> would, until
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
	var datapack promptDatapack
	code := exitOK
	for {
		if interactive {
			label := datapack.name
			if label == "" {
				label = "default"
			}
			fmt.Fprintf(stdout, "mekosh %s> ", label)
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
			fmt.Fprintf(stderr, "mekosh: %v\n", werr)
			lc = exitUsage
		case len(words) == 0:
		case words[0] == "exit" || words[0] == "quit":
			return code
		case words[0] == "use":
			lc = promptUse(l, words[1:], &datapack, stdout, stderr)
		default:
			lc = runProg(progMekosh, promptArgs(words, datapack.id, dataDir), stdout, stderr)
			// The datapack in use is gone: later lines go to the default one again.
			if lc == exitOK && words[0] == "delete_datapack" && datapack.id != "" {
				if named := firstArg(words[1:]); named == datapack.id || named == datapack.name {
					datapack = promptDatapack{}
					fmt.Fprintln(stdout, "Using the default datapack")
				}
			}
		}
		if lc != exitOK && code == exitOK && !interactive {
			code = lc
		}
	}
}

// promptArgs adds what the prompt knows to a line's words: the datapack use set, for a command that
// takes --datapack and names none, and the data folder mekosh was given. help takes neither: its
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
func promptUse(l paths.Layout, args []string, datapack *promptDatapack, stdout, stderr io.Writer) int {
	switch len(args) {
	case 0:
		if datapack.id == "" {
			fmt.Fprintln(stdout, "Using the default datapack")
		} else {
			fmt.Fprintf(stdout, "Using datapack %s\n", datapack.name)
		}
		return exitOK
	case 1:
	default:
		fmt.Fprintln(stderr, "mekosh: use takes one datapack, by name or id")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	t, err := connectTools(ctx, l)
	if err != nil {
		fmt.Fprintf(stderr, "mekosh: the server is running, but %v\n", err)
		return exitError
	}
	defer func() { _ = t.s.Close() }()
	id, name, err := t.findDatapack(ctx, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "mekosh: %v\n", err)
		return exitError
	}
	*datapack = promptDatapack{id: id, name: name}
	fmt.Fprintf(stdout, "Using datapack %s\n", name)
	return exitOK
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
