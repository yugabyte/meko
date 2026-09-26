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
	"errors"
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
			// The universe's TLS shape, so a deploy script can decide what it
			// has to stand up FIRST — a cert-manager universe needs its issuer
			// to exist before YBA creates the universe, and that is knowable
			// from the spec alone. It keeps TLS from becoming a second input
			// that can disagree with this file.
			"tls": spec.TLS(),
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
	// An aborted converge names the steps it skipped; only here is the spec
	// path known, so the resume command is spelled out here.
	//
	// It echoes the EFFECTIVE namespace and context, not just the spec path.
	// The spec file stays checked in while CI supplies the namespace per run
	// (amp/deploy/up-gke.sh passes --namespace and --kube-context), so a
	// resume command without them names a different foundation than the one
	// that aborted — and running it would stand a second one up.
	//
	// Namespace needs no emptiness guard: Validate requires one, and load runs
	// it before anything here. The context does — it is legitimately empty,
	// meaning whatever context is current.
	var incomplete *foundation.IncompleteError
	if errors.As(err, &incomplete) {
		resume := fmt.Sprintf("foundation apply -c %s --namespace %s", *c.specPath, spec.Namespace)
		if spec.KubeContext != "" {
			resume += " --kube-context " + spec.KubeContext
		}
		if *noPrune {
			resume += " --no-prune"
		}
		return fmt.Errorf("%w\n  %s", err, resume)
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
	// The consumer's own libpq parameters. Flags rather than spec fields: these
	// are paths inside the CALLER's container, which the universe knows nothing
	// about and two consumers need not agree on. See foundation.DSNOptions.
	sslMode := fs.String("sslmode", "",
		"override the sslmode the universe implies (disable | require | verify-ca | verify-full)")
	sslRootCert := fs.String("sslrootcert", "", "CA bundle path, in the CONSUMER's filesystem")
	sslCert := fs.String("sslcert", "", "client certificate path, in the CONSUMER's filesystem")
	sslKey := fs.String("sslkey", "", "client key path, in the CONSUMER's filesystem")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// A certificate is a pair. Half of one is a DSN that parses and fails at
	// the first connection, which is the kind of thing this command exists to
	// keep out of a deploy script.
	if (*sslCert == "") != (*sslKey == "") {
		return fmt.Errorf("--sslcert and --sslkey go together: a client certificate without its key " +
			"authenticates nothing")
	}
	spec, err := c.load()
	if err != nil {
		return err
	}
	d := deployer(spec).WithDSNOptions(foundation.DSNOptions{
		SSLMode:     *sslMode,
		SSLRootCert: *sslRootCert,
		SSLCert:     *sslCert,
		SSLKey:      *sslKey,
	})
	// The caller and the spec have to agree about TLS, in both directions.
	// Either mismatch yields a DSN that fails at the first connection, with
	// nothing in the message to say which half was wrong.
	switch mode := d.SSLMode(); {
	case *sslRootCert != "" && mode == "disable":
		return fmt.Errorf("--sslrootcert was given but this universe has enableClientToNodeEncrypt "+
			"false, so the DSN would be sslmode=disable: the certificate paths would be ignored "+
			"and the connection would be plaintext. Turn client TLS on in %s, or drop the flags",
			*c.specPath)
	case *sslRootCert == "" && mode == "verify-full":
		// libpq would fall back to ~/.postgresql/root.crt, which is not a file
		// any consumer of this DSN has.
		return fmt.Errorf("this universe names a rootCA, so its DSN is sslmode=verify-full and the " +
			"client must verify the server against that CA — but no --sslrootcert was given, and " +
			"libpq would look for ~/.postgresql/root.crt. Pass the CA path as the consumer sees " +
			"it, or --sslmode require to encrypt without verifying")
	}
	info, err := d.Connect(ctx)
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
