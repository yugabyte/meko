package mekostack

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	FindingPodRestart = "pod_restart"
	FindingCrashLog   = "crash_log"
	FindingHTTP5xx    = "http_5xx"
	FindingErrorLogs  = "error_logs"
	FindingUnread     = "guard_unread"

	apiMetricsPort = 9464
	// Log lines carry the container's clock, and --since-time is inclusive, so a
	// little slack keeps a line written at the mark from being missed.
	logSlack = 2 * time.Second
)

// serviceSelectors are the meko workloads whose logs the guard reads.
var serviceSelectors = []string{
	"app=" + apiDeployment,
	"app=" + mcpService,
	"app=" + langfuseDeployment,
	"app.kubernetes.io/name=inference-gateway",
	"app.kubernetes.io/name=rag-agent",
}

var (
	crashPatterns = regexp.MustCompile(`panic: |fatal error: |Traceback \(most recent call last\)|\[signal SIG(SEGV|BUS|ABRT)|goroutine \d+ \[running\]:`)
	errorPattern  = regexp.MustCompile(`level=ERROR|"level":"error"|\] ERROR |\bERROR:`)
	requestsLine  = regexp.MustCompile(`^meko_api_http_requests_total\{(.*)\} ([0-9.eE+-]+)$`)
	labelPair     = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// Finding is one thing a service did wrong between a mark and a check.
type Finding struct {
	Kind     string
	Workload string
	Detail   string
	Fatal    bool
}

// GuardMark is where the stack's services stood before a test body.
type GuardMark struct {
	At       time.Time
	Restarts map[string]int
	Requests map[string]float64
	// Unread are api-server pods whose metrics the mark could not read: their
	// counters at the check are a lifetime total, not a delta.
	Unread map[string]bool
}

// GuardMark records container restart counts and the api-server's request
// counters.
func (a *App) GuardMark(ctx context.Context) (GuardMark, error) {
	restarts, _, err := a.restartCounts(ctx)
	if err != nil {
		return GuardMark{}, err
	}
	requests, unread := a.requestCounts(ctx)
	return GuardMark{At: a.now(), Restarts: restarts, Requests: requests, Unread: unread}, nil
}

// GuardCheck reports containers that restarted, crashes in service logs, 5xx
// answers the api-server served, and how many ERROR lines the services logged,
// all since the mark.
func (a *App) GuardCheck(ctx context.Context, m GuardMark) ([]Finding, error) {
	var out []Finding
	restarts, reasons, err := a.restartCounts(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, restartFindings(m.Restarts, restarts, reasons)...)
	requests, unread := a.requestCounts(ctx)
	for pod := range m.Unread {
		for k := range requests {
			if strings.HasPrefix(k, pod+" ") {
				delete(requests, k)
			}
		}
	}
	out = append(out, requestFindings(m.Requests, requests)...)
	if missed := sortedKeys(m.Unread, unread); len(missed) > 0 {
		out = append(out, Finding{Kind: FindingUnread, Workload: apiDeployment, Detail: "metrics unreadable on " + strings.Join(missed, ", "), Fatal: true})
	}

	since := m.At.Add(-logSlack)
	perSelector := make([][]Finding, len(serviceSelectors))
	var wg sync.WaitGroup
	for i, sel := range serviceSelectors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			logs, err := a.kube.LogsSince(ctx, sel, since)
			if err != nil {
				logs, err = a.kube.LogsSince(ctx, sel, since)
			}
			if err != nil {
				perSelector[i] = []Finding{{Kind: FindingUnread, Workload: sel, Detail: "logs unreadable: " + truncateLine(err.Error()), Fatal: true}}
				return
			}
			perSelector[i] = logFindings(sel, logs)
		}()
	}
	wg.Wait()
	for _, fs := range perSelector {
		out = append(out, fs...)
	}
	return out, nil
}

// restartCounts maps pod/container to its restart count, and to the reason its
// last instance ended.
func (a *App) restartCounts(ctx context.Context) (map[string]int, map[string]string, error) {
	raw, err := a.kube.GetSelector(ctx, "pods", "",
		`{range .items[*]}{.metadata.name}{"\t"}{range .status.containerStatuses[*]}{.name}{"="}{.restartCount}{"="}{.lastState.terminated.reason}{","}{end}{"\n"}{end}`)
	if err != nil {
		return nil, nil, err
	}
	counts, reasons := parseRestarts(raw)
	return counts, reasons, nil
}

func parseRestarts(raw string) (map[string]int, map[string]string) {
	counts, reasons := map[string]int{}, map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		pod, rest, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		for _, c := range strings.Split(rest, ",") {
			f := strings.SplitN(c, "=", 3)
			if len(f) < 2 || f[0] == "" {
				continue
			}
			n, _ := strconv.Atoi(f[1])
			key := pod + "/" + f[0]
			counts[key] = n
			if len(f) == 3 && f[2] != "" {
				reasons[key] = f[2]
			}
		}
	}
	return counts, reasons
}

func restartFindings(before, after map[string]int, reasons map[string]string) []Finding {
	var out []Finding
	keys := make([]string, 0, len(after))
	for k := range after {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		delta := after[k] - before[k]
		if delta <= 0 {
			continue
		}
		detail := fmt.Sprintf("restarted %d time(s)", delta)
		if r := reasons[k]; r != "" {
			detail += ", last exit " + r
		}
		out = append(out, Finding{Kind: FindingPodRestart, Workload: k, Detail: detail, Fatal: true})
	}
	return out
}

// requestCounts sums meko_api_http_requests_total per pod, route and status
// across the api-server pods, read through the API server's pod proxy so it
// works wherever kubectl does. A pod it cannot read twice is returned as unread.
func (a *App) requestCounts(ctx context.Context) (map[string]float64, map[string]bool) {
	out, unread := map[string]float64{}, map[string]bool{}
	pods, err := a.kube.GetSelector(ctx, "pods", "app="+apiDeployment, `{range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		unread["(pod list)"] = true
		return out, unread
	}
	for _, pod := range strings.Fields(pods) {
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:%d/proxy/metrics", a.cfg.Namespace, pod, apiMetricsPort)
		body, err := a.kube.GetRaw(ctx, path)
		if err != nil {
			body, err = a.kube.GetRaw(ctx, path)
		}
		if err != nil {
			unread[pod] = true
			continue
		}
		for k, v := range parseRequests(body) {
			out[pod+" "+k] = v
		}
	}
	return out, unread
}

func sortedKeys(sets ...map[string]bool) []string {
	seen := map[string]bool{}
	for _, s := range sets {
		for k := range s {
			seen[k] = true
		}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// parseRequests keys each 5xx series by "METHOD route status".
func parseRequests(body string) map[string]float64 {
	out := map[string]float64{}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		m := requestsLine.FindStringSubmatch(strings.TrimSpace(sc.Text()))
		if m == nil {
			continue
		}
		labels := map[string]string{}
		for _, p := range labelPair.FindAllStringSubmatch(m[1], -1) {
			labels[p[1]] = p[2]
		}
		if !strings.HasPrefix(labels["status"], "5") {
			continue
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		out[labels["method"]+" "+labels["route"]+" "+labels["status"]] = v
	}
	return out
}

func requestFindings(before, after map[string]float64) []Finding {
	totals := map[string]float64{}
	for k, v := range after {
		delta := v - before[k]
		if delta < 0 {
			delta = v
		}
		if delta <= 0 {
			continue
		}
		_, series, _ := strings.Cut(k, " ")
		totals[series] += delta
	}
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []Finding
	for _, k := range keys {
		out = append(out, Finding{Kind: FindingHTTP5xx, Workload: apiDeployment, Detail: fmt.Sprintf("%s answered %g time(s)", k, totals[k]), Fatal: true})
	}
	return out
}

func logFindings(selector, logs string) []Finding {
	var out []Finding
	var crashes []string
	errors := 0
	var firstError string
	for _, line := range strings.Split(logs, "\n") {
		switch {
		case crashPatterns.MatchString(line):
			if len(crashes) < 5 {
				crashes = append(crashes, truncateLine(line))
			}
		case errorPattern.MatchString(line):
			errors++
			if firstError == "" {
				firstError = truncateLine(line)
			}
		}
	}
	if len(crashes) > 0 {
		out = append(out, Finding{Kind: FindingCrashLog, Workload: selector, Detail: strings.Join(crashes, " | "), Fatal: true})
	}
	if errors > 0 {
		out = append(out, Finding{Kind: FindingErrorLogs, Workload: selector, Detail: fmt.Sprintf("%d ERROR line(s), first: %s", errors, firstError)})
	}
	return out
}

func truncateLine(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}
