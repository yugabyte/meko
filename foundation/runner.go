package foundation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// Runner runs one external command. Every kubectl and helm invocation in this
// package goes through it, which is what makes the deployer testable without a
// cluster: the tests inject Recorder and assert on exact argv and rendered
// manifests.
//
// Result carries the exit code rather than only an error so a caller can treat
// an expected non-zero (a `kubectl get` on something absent) as data.
type Runner func(ctx context.Context, stdin []byte, argv []string) (Result, error)

// Result is one command's outcome.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Exec is the real Runner: it shells the ambient-authed binary. kubectl and
// helm stay external binaries rather than linked SDKs — the ampqa convention —
// which keeps this module's dependency list to a YAML parser and means the
// deployer authenticates exactly the way the operator's own kubectl does.
func Exec(ctx context.Context, stdin []byte, argv []string) (Result, error) {
	if len(argv) == 0 {
		return Result{}, fmt.Errorf("empty argv")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	if err != nil {
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			// A non-zero exit is an outcome, not a failure to run. The caller
			// decides, because "absent" and "broken" are both non-zero and only
			// the caller knows which it asked about.
			return res, nil
		}
		return res, fmt.Errorf("run %s: %w", strings.Join(argv, " "), err)
	}
	return res, nil
}

// Call is one recorded invocation.
type Call struct {
	Argv  []string
	Stdin string
}

// Recorder is a Runner that records what it was asked to run and replays canned
// results. Tests assert on Calls; Responses maps an argv substring to the
// Result to return, so a test only has to describe the commands it cares about.
type Recorder struct {
	mu    sync.Mutex
	Calls []Call
	// Responses is consulted in order: the first key that is a substring of the
	// joined argv wins. An unmatched command returns exit 0 and empty output,
	// which is the right default for an apply.
	Responses []Response
}

// Response is a canned result for commands whose argv contains Match.
type Response struct {
	Match  string
	Result Result
	Err    error
	// Once limits this response to a single use, so a test can script a poll
	// that changes answer between ticks.
	Once bool
	used bool
}

// Run implements Runner.
func (r *Recorder) Run(ctx context.Context, stdin []byte, argv []string) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Calls = append(r.Calls, Call{Argv: argv, Stdin: string(stdin)})
	joined := strings.Join(argv, " ")
	for i := range r.Responses {
		resp := &r.Responses[i]
		if resp.Once && resp.used {
			continue
		}
		if strings.Contains(joined, resp.Match) {
			resp.used = true
			return resp.Result, resp.Err
		}
	}
	return Result{}, nil
}

func (r *Recorder) Runner() Runner { return r.Run }

// Argvs returns every recorded argv joined, for assertions.
func (r *Recorder) Argvs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.Calls))
	for _, c := range r.Calls {
		out = append(out, strings.Join(c.Argv, " "))
	}
	return out
}
