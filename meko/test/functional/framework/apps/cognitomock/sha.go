package cognitomock

import (
	"crypto/sha256"
	"encoding/hex"
)

// sha names the inputs the pod mounts, so a changed binary argument or certificate rolls
// the pod on a re-apply instead of leaving it on the old files.
func sha(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
