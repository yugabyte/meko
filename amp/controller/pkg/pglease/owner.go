package pglease

import (
	"crypto/rand"
	"fmt"
	"os"
)

// NewOwner returns an owner id unique to this process: label, hostname, pid and
// a random nonce.
//
// An owner id must identify one PROCESS, not one logical service. Acquire grants
// unconditionally to the current holder — that is what makes renewal cheap — so
// two processes sharing an id are both "the holder", both get granted, and the
// mutual exclusion is gone. The nonce is what makes a restarted pod with the
// same name and a recycled pid a different owner.
func NewOwner(label string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("%s@%s/%d/%s", label, host, os.Getpid(), rand.Text()[:10])
}
