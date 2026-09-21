// Command foundation renders and applies a YugabyteDB foundation: one
// operator-managed universe, reachable on the ClusterIP Service its CR asks the
// YugabyteDB chart to create.
//
// The spec file is the source of truth. `render` turns it into manifests and
// touches nothing; `up` and `apply` send those to a cluster and wait. An update
// is an edit to the file and another `apply` — there is no separate mutation
// surface, because the YBUniverse CR already is one.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"meko/foundation"
)

func main() {
	// A bring-up runs for tens of minutes, so Ctrl-C has to reach the waits
	// rather than leaving a half-applied foundation with no way to interrupt it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "foundation: %v\n", err)
		os.Exit(1)
	}
}

const usage = `foundation — stand up and converge a YugabyteDB foundation

  foundation render   -c SPEC [-o DIR]   render manifests; applies nothing
  foundation validate -c SPEC            parse and check the spec
  foundation up       -c SPEC            first bring-up: CRDs, yugaware, universe, databases
  foundation apply    -c SPEC            converge an existing foundation onto the spec
  foundation status   -c SPEC            the universe's state, as JSON
  foundation connstr  -c SPEC [--shell]  connection details, as JSON or shell exports
  foundation down     -c SPEC [flags]    delete the universe, the release and its cluster RBAC

Common flags: --kube-context, --namespace (override the spec).
`

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return fmt.Errorf("a verb is required")
	}
	verb, rest := args[0], args[1:]
	switch verb {
	case "render":
		return cmdRender(rest)
	case "validate":
		return cmdValidate(rest)
	case "up":
		return cmdConverge(ctx, rest, true)
	case "apply":
		return cmdConverge(ctx, rest, false)
	case "status":
		return cmdStatus(ctx, rest)
	case "connstr":
		return cmdConnstr(ctx, rest)
	case "down":
		return cmdDown(ctx, rest)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown verb %q", verb)
	}
}

// common registers the flags every verb accepts and returns a loader that
// applies the overrides. --kube-context and --namespace exist because CI passes
// them per-run while the spec file stays checked in.
type common struct {
	specPath    *string
	kubeContext *string
	namespace   *string
}

func commonFlags(fs *flag.FlagSet) *common {
	return &common{
		specPath:    fs.String("c", "", "path to the foundation spec YAML (required)"),
		kubeContext: fs.String("kube-context", "", "override spec.kubeContext"),
		namespace:   fs.String("namespace", "", "override spec.namespace"),
	}
}

func (c *common) load() (*foundation.Spec, error) {
	if *c.specPath == "" {
		return nil, fmt.Errorf("-c SPEC is required")
	}
	data, err := os.ReadFile(*c.specPath)
	if err != nil {
		return nil, err
	}
	spec, err := foundation.Load(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", *c.specPath, err)
	}
	if *c.kubeContext != "" {
		spec.KubeContext = *c.kubeContext
	}
	if *c.namespace != "" {
		spec.Namespace = *c.namespace
	}
	// --namespace can move the universe, so the zone rule is re-checked.
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}

// deployer builds a Deployer that logs progress to stderr, keeping stdout clean
// for the JSON and shell output the other verbs emit.
func deployer(spec *foundation.Spec) *foundation.Deployer {
	return foundation.New(spec, nil).WithLogger(func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "==> "+format+"\n", args...)
	})
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	c := commonFlags(fs)
	outDir := fs.String("o", "", "write manifests to this directory (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	manifests, err := spec.Render()
	if err != nil {
		return err
	}
	if *outDir == "" {
		for _, m := range manifests {
			fmt.Printf("--- %s\n%s", m.Name, m.JSON)
		}
		return nil
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	for _, m := range manifests {
		p := filepath.Join(*outDir, m.Name)
		if err := os.WriteFile(p, m.JSON, 0o644); err != nil {
			return err
		}
		fmt.Println(p)
	}
	// The CRDs are cluster-scoped and shared, and are applied by version rather
	// than rendered, so name the URL instead of silently omitting the step.
	fmt.Printf("\nCRDs (cluster-scoped, apply once per cluster):\n  kubectl apply --server-side -f %s\n",
		spec.CRDSource())
	return nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	c := commonFlags(fs)
	asJSON := fs.Bool("json", false, "emit the checked spec's facts as JSON instead of a line of prose")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	// Render is called for its side effect of exercising the whole path — the
	// defaults, the provider/universe bodies, the marshal. It does NOT check the
	// bodies against the CRD's schema: a passthrough typo passes here and is
	// rejected by the API server on apply.
	manifests, err := spec.Render()
	if err != nil {
		return err
	}
	kinds := make([]string, 0, len(manifests))
	for _, m := range manifests {
		kinds = append(kinds, m.Kind)
	}
	// --json so a caller can act on the spec without parsing it: the storage
	// classes in particular are set in two places and the provider's wins, so
	// anything reading the YAML itself would check the wrong class.
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"universe":  spec.UniverseName(),
			"namespace": spec.Namespace,
			// Where the DB pods and the Service consumers dial actually land —
			// the provider zones' kubeNamespace, which need not be the
			// foundation's namespace.
			"universeNamespace": spec.UniverseNamespace(),
			"chartVersion":      spec.ChartVersion,
			"kinds":             kinds,
			"storageClasses":    spec.StorageClasses(),
		})
	}
	fmt.Printf("ok: %s in namespace %s, chart %s — renders %s\n",
		spec.UniverseName(), spec.Namespace, spec.ChartVersion, strings.Join(kinds, " + "))
	return nil
}

func cmdConverge(ctx context.Context, args []string, initial bool) error {
	name := "apply"
	if initial {
		name = "up"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := commonFlags(fs)
	ybaTimeout := fs.Duration("yba-timeout", 0, "how long to wait for the yugaware install")
	univTimeout := fs.Duration("universe-timeout", 0, "how long to wait for the universe to settle")
	shell := fs.Bool("shell", false, "emit connection details as shell exports instead of JSON")
	noPrune := fs.Bool("no-prune", false,
		"leave operator CRs the spec no longer declares on the cluster")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	d := deployer(spec).WithTimeouts(*ybaTimeout, *univTimeout, 0)
	if *noPrune {
		d = d.WithoutPrune()
	}

	start := time.Now()
	var info *foundation.Info
	if initial {
		info, err = d.Up(ctx)
	} else {
		info, err = d.Apply(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "==> done in %s\n", time.Since(start).Round(time.Second))
	return emit(info, *shell)
}

func cmdStatus(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	c := commonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	info, err := deployer(spec).Connect(ctx)
	if err != nil {
		return err
	}
	return emit(info, false)
}

func cmdConnstr(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("connstr", flag.ContinueOnError)
	c := commonFlags(fs)
	shell := fs.Bool("shell", false, "emit shell exports for `eval` instead of JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	info, err := deployer(spec).Connect(ctx)
	if err != nil {
		return err
	}
	return emit(info, *shell)
}

func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ContinueOnError)
	c := commonFlags(fs)
	// Both default off because both are destructive beyond this foundation: the
	// namespace is shared with the consumer's workloads, and the CRDs are
	// cluster-scoped and shared with every other foundation on the cluster.
	deleteNS := fs.Bool("delete-namespace", false,
		"also delete the namespace (it is SHARED with the consumer's workloads)")
	deleteCRDs := fs.Bool("delete-crds", false,
		"also delete the operator CRDs (cluster-scoped: this deletes every other foundation's universe too)")
	finalizer := fs.Duration("finalizer-timeout", 0, "how long to wait for the CR's finalizer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	return deployer(spec).Down(ctx, foundation.DownOptions{
		DeleteNamespace:  *deleteNS,
		DeleteCRDs:       *deleteCRDs,
		FinalizerTimeout: *finalizer,
	})
}

// emit writes Info to stdout, as shell exports for `eval` or as JSON.
func emit(info *foundation.Info, shell bool) error {
	if shell {
		fmt.Print(info.ShellExports())
		return nil
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
