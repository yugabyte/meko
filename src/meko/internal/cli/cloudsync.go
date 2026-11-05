package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/cloudsync"
	"github.com/yugabyte/meko/src/meko/internal/paths"
)

// syncPoll is how often meko cloud_sync asks how its sync is going.
var syncPoll = 2 * time.Second

// runCloudSync is meko cloud_sync <datapack>: one of this Meko's datapacks moved to the linked Meko
// Cloud, through the running server, waiting until it's there or has failed.
func runCloudSync(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("meko cloud_sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	dataDir := fs.String("data-dir", "", "the data folder (default ~/.meko, or MEKO_HOME)")
	check := fs.Bool("check", false, "only check whether it fits your Meko Cloud plan")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	err := fs.Parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		c, _ := lookup(progMeko, "cloud_sync")
		printCommandHelp(stdout, progMeko, c, fs)
		return exitOK
	case err != nil:
		fmt.Fprintln(stderr, "See meko cloud_sync --help")
		return exitUsage
	case name == "" && fs.NArg() == 0:
		c, _ := lookup(progMeko, "cloud_sync")
		printCommandHelp(stdout, progMeko, c, fs)
		return exitUsage
	}
	if name == "" {
		name = fs.Arg(0)
	}
	root, err := paths.Resolve(*dataDir)
	if err != nil {
		return fail(stderr, err)
	}
	c, err := newClient(paths.New(root))
	if err != nil {
		return fail(stderr, fmt.Errorf("%w (is Meko running? meko start)", err))
	}
	ctx := context.Background()
	id, err := localDatapack(ctx, c, name)
	if err != nil {
		return fail(stderr, err)
	}
	base := "/api/v1/datapacks/" + url.PathEscape(id) + "/cloud-sync"
	var pc cloudsync.Precheck
	if err = c.send(ctx, http.MethodPost, base+"/precheck", nil, &pc); err != nil {
		return fail(stderr, err)
	}
	printPrecheck(stdout, name, pc)
	if !pc.OK || *check {
		if !pc.OK {
			return exitError
		}
		return exitOK
	}
	var run cloudsync.Run
	if err = c.send(ctx, http.MethodPost, base, nil, &run); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Syncing %s to Meko Cloud ", name)
	for run.State == cloudsync.StateSyncing {
		time.Sleep(syncPoll)
		fmt.Fprint(stdout, "·")
		if err = c.get(ctx, base, &run); err != nil {
			fmt.Fprintln(stdout)
			return fail(stderr, err)
		}
	}
	fmt.Fprintln(stdout)
	if run.State != cloudsync.StateDone {
		return fail(stderr, fmt.Errorf("%s wasn't synced, and it's still here as it was: %s", name, run.Error))
	}
	fmt.Fprintf(stdout, "%s is in Meko Cloud now, and no longer on this computer\n", name)
	if run.WebURL != "" {
		fmt.Fprintf(stdout, "Open it: %s\n", run.WebURL)
	}
	return exitOK
}

// printPrecheck says how a datapack measures up to each of the plan's limits.
func printPrecheck(w io.Writer, name string, pc cloudsync.Precheck) {
	verdict := "fits"
	if !pc.OK {
		verdict = "doesn't fit"
	}
	fmt.Fprintf(w, "%s %s your Meko Cloud %s plan:\n", name, verdict, pc.Plan)
	for _, ch := range pc.Checks {
		mark, limit := "✓", "no limit"
		if !ch.OK {
			mark = "✗"
		}
		if ch.Limit >= 0 {
			limit = fmt.Sprintf("limit %d", ch.Limit)
		}
		fmt.Fprintf(w, "  %s %-17s %d used + %d = %d (%s)\n", mark, ch.Check, ch.Used, ch.Adding, ch.Used+ch.Adding, limit)
		if ch.Message != "" && !ch.OK {
			fmt.Fprintf(w, "    %s\n", ch.Message)
		}
	}
	if !pc.OK {
		fmt.Fprintln(w, "Upgrade your Meko Cloud plan, or delete datapacks there, to sync it")
	}
}

// localDatapack is the id of this Meko's datapack named name (or with the id name).
func localDatapack(ctx context.Context, c *client, name string) (string, error) {
	var rows []struct {
		ID     string `json:"datapack_id"`
		Name   string `json:"datapack_name"`
		WebURL string `json:"web_url"`
	}
	if err := c.get(ctx, "/api/v1/datapacks", &rows); err != nil {
		return "", err
	}
	for _, r := range rows {
		if (r.Name == name || strings.EqualFold(r.ID, name)) && strings.HasPrefix(r.WebURL, "/") {
			return r.ID, nil
		}
		if r.Name == name || strings.EqualFold(r.ID, name) {
			return "", fmt.Errorf("%s is already in Meko Cloud", name)
		}
	}
	return "", fmt.Errorf("no datapack called %s on this computer: meko-cli datapacks lists them", name)
}
