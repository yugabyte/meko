package mekoapi

import "regexp"

var internalEndpoints = []*regexp.Regexp{
	regexp.MustCompile(`[a-z0-9-]+(\.[a-z0-9-]+)*\.svc(\.cluster\.local)?\.?(:\d+)?\b`),
	regexp.MustCompile(`[a-z0-9.-]+\.elb(\.[a-z0-9-]+)?\.amazonaws\.com(:\d+)?`),
	regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}:\d{2,5}\b`),
}

// InternalLeaks returns the in-cluster service names, cloud load balancer
// hostnames and ip:port addresses s exposes. None of them belong in a message a
// user can see.
func InternalLeaks(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range internalEndpoints {
		for _, m := range re.FindAllString(s, -1) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}
