package k8s

import (
	"strings"
	"time"
)

// APIServerUnreachableError marks a kubectl call that never reached the API server.
type APIServerUnreachableError struct{ Err error }

func (e *APIServerUnreachableError) Error() string { return e.Err.Error() }
func (e *APIServerUnreachableError) Unwrap() error { return e.Err }

// kubectl words a refused API server differently from every other transport failure.
func apiServerUnreachable(out string) bool {
	low := strings.ToLower(out)
	return strings.Contains(low, "unable to connect to the server") ||
		(strings.Contains(low, "the connection to the server") && strings.Contains(low, "was refused")) ||
		strings.Contains(low, "error upgrading connection: dial tcp")
}

// kubectl prints "Unable to connect to the server" for any transport error,
// including an EOF after the exec upgrade may already have started the command.
// Only these prove the request never left the host.
func apiServerNeverDialed(out string) bool {
	low := strings.ToLower(out)
	for _, m := range []string{"dial tcp", "connection refused", "no such host", "tls handshake timeout"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// KubeletUnreachableError marks a kubectl stream (port-forward, cp, exec) the API
// server could not carry to the pod's kubelet, after retries.
type KubeletUnreachableError struct{ Err error }

func (e *KubeletUnreachableError) Error() string { return e.Err.Error() }
func (e *KubeletUnreachableError) Unwrap() error { return e.Err }

// A node the autoscaler just added refuses kubelet dials for its first minute or
// two on amp-dev; 10 attempts wait 135s in total.
const kubeletDialAttempts = 10

// kubeletRetryDelay is multiplied by the attempt number; a var so tests need not wait.
var kubeletRetryDelay = 3 * time.Second

// The API server could not reach the node's kubelet, so the exec never started.
// kubectl prints its container-default notice ahead of that error.
func kubeletUnreachable(stderr string) bool {
	for _, l := range strings.Split(stderr, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "Defaulted container ") {
			return strings.HasPrefix(l, "Error from server: error dialing backend")
		}
	}
	return false
}

// kubectl port-forward and cp print no remote output, so a substring is safe here.
func kubeletStreamFailed(out string) bool {
	if apiServerUnreachable(out) {
		return false
	}
	low := strings.ToLower(out)
	for _, m := range []string{"error dialing backend", "i/o timeout", "use of closed network connection", "error: context deadline exceeded"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

const webhookAttempts = 5

// The API server reaches an admission webhook's pod the same way it reaches a
// kubelet, so a fresh node times the call out too. Apply is idempotent.
func webhookUnreachable(stderr string) bool {
	low := strings.ToLower(stderr)
	return strings.Contains(low, "failed calling webhook") &&
		(strings.Contains(low, "context deadline exceeded") || strings.Contains(low, "i/o timeout"))
}
