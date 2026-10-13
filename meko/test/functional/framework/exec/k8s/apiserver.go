package k8s

import "strings"

// APIServerUnreachableError marks a kubectl call that never reached the API server.
type APIServerUnreachableError struct{ Err error }

func (e *APIServerUnreachableError) Error() string { return e.Err.Error() }
func (e *APIServerUnreachableError) Unwrap() error { return e.Err }

// kubectl words a refused API server differently from every other transport failure.
func apiServerUnreachable(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "unable to connect to the server") ||
		(strings.Contains(low, "the connection to the server") && strings.Contains(low, "was refused"))
}

// kubectl prints "Unable to connect to the server" for any transport error,
// including an EOF after the exec upgrade may already have started the command.
// Only these prove the request never left the host.
func apiServerNeverDialed(out string) bool {
	low := strings.ToLower(out)
	for _, m := range []string{"dial tcp", "connection refused", "was refused", "no such host", "tls handshake timeout"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func kubectlNeverDialed(out string) bool {
	return apiServerUnreachable(out) && apiServerNeverDialed(out)
}
