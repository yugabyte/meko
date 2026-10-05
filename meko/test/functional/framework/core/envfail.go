package core

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
)

// Env-failure marking for tests that provision their OWN backing inside the
// body, where a plain error would land as `failed`, a product signal.

// EnvError marks an ENVIRONMENT failure transparently: Error() is the wrapped
// text and Unwrap keeps errors.Is/As working.
type EnvError struct{ Err error }

func (e *EnvError) Error() string { return e.Err.Error() }
func (e *EnvError) Unwrap() error { return e.Err }

// EnvFail formats an env failure, fmt.Errorf-style (%w keeps the cause).
func EnvFail(format string, args ...any) error {
	return &EnvError{Err: fmt.Errorf(format, args...)}
}

// AsEnvFailure marks an existing error, text unchanged; nil passes through.
func AsEnvFailure(err error) error {
	if err == nil || IsEnvFailure(err) {
		return err
	}
	return &EnvError{Err: err}
}

// IsEnvFailure reports whether err (or anything it wraps) is an EnvError.
func IsEnvFailure(err error) bool {
	var ee *EnvError
	return errors.As(err, &ee)
}

// neverRetry causes fail identically every attempt. Checked FIRST, so a message
// carrying one of these AND a scheduling phrase never retries.
var neverRetry = []string{
	"imagepullbackoff",
	"errimagepull",
	"invalidimagename",
	"manifest unknown",
	"manifest for",
	"pull access denied",
	"repository does not exist",
	"unauthorized",
	"forbidden",
	"invalid credentials",
	"x509",
	"no such file or directory",
	// A namespace that was deleted never comes back; retrying burns the backoff
	// and a whole attempt to fail identically.
	sharedInfraGoneMarker,
}

// sharedInfraGoneMarker is appended to a verdict reached on a deleted stack.
const sharedInfraGoneMarker = "was deleted mid-run"

// schedulingSignals are the ONLY retryable failures. Every token of an entry must
// appear, pinning "context deadline exceeded" to a readiness wait; specific first.
var schedulingSignals = []struct {
	reason string
	all    []string
}{
	{"kube api server unreachable", []string{"kubectl: unable to connect to the server"}},
	{"kube api server unreachable", []string{"kubectl: the connection to the server", "was refused"}},
	{"pod unschedulable (insufficient cpu)", []string{"insufficient cpu"}},
	{"pod unschedulable (insufficient memory)", []string{"insufficient memory"}},
	{"pod unschedulable (persistentvolume node affinity)", []string{"persistentvolume's node affinity"}},
	{"pod unschedulable", []string{"unschedulable"}},
	{"no node could take the pod", []string{"nodes are available"}},
	{"helm install timed out waiting for readiness", []string{"helm install", "context deadline exceeded"}},
	{"helm install timed out waiting for readiness", []string{"not ready", "context deadline exceeded"}},
	{"readiness wait timed out", []string{"timed out waiting for the condition"}},
	{"readiness wait timed out", []string{"never became ready"}},
	// A product bug, retried because a fresh stack usually gets past it: the
	// RETRY line names it, so it stays visible rather than absorbed.
	{"api-server migrations wedged (golang-migrate postgres driver on YB)", []string{"api-server migrations wedged on a dirty version"}},
	{"readiness wait timed out", []string{"not ready before"}},
}

// KubeAPIUnreachable is true only when an unreachable API server is the WHOLE failure, never one branch of a join.
func KubeAPIUnreachable(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		branches := joined.Unwrap()
		if len(branches) == 0 {
			return false
		}
		for _, b := range branches {
			if !KubeAPIUnreachable(b) {
				return false
			}
		}
		return true
	}
	var ae *k8s.APIServerUnreachableError
	if errors.As(err, &ae) {
		return !hasJoin(err)
	}
	return false
}

// errors.As walks past a join, so a wrapped one has to be rejected explicitly.
func hasJoin(err error) bool {
	for err != nil {
		if _, ok := err.(interface{ Unwrap() []error }); ok {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// SchedulingReason: narrow by design, product, auth and image errors must not match.
func SchedulingReason(msg string) (string, bool) {
	low := strings.ToLower(msg)
	if low == "" {
		return "", false
	}
	for _, marker := range neverRetry {
		if strings.Contains(low, marker) {
			return "", false
		}
	}
	for _, sig := range schedulingSignals {
		if containsAll(low, sig.all) {
			return sig.reason, true
		}
	}
	return "", false
}

func containsAll(s string, tokens []string) bool {
	for _, tok := range tokens {
		if !strings.Contains(s, tok) {
			return false
		}
	}
	return true
}
