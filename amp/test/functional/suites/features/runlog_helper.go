package features

import (
	"bytes"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/yugabyte/amp/test/functional/framework/core"
)

// runLog tees the flow's (already-masked) log output into an in-memory buffer so
// the whole run's framework log — every step, and every kubectl/helm command +
// truncated output — can be attached to the report, while still echoing to
// stderr for the live Jenkins console.
type runLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *runLog) printf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	r.mu.Lock()
	r.buf.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		r.buf.WriteByte('\n')
	}
	r.mu.Unlock()
	log.Printf("%s", line)
}

func (r *runLog) bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf.Bytes()...)
}

// newFlowLog builds the flow's logf (masking base) plus the recorder backing it,
// so the caller can attach the captured log at teardown.
func newFlowLog(m *core.Masker) (func(string, ...any), *runLog) {
	rec := &runLog{}
	return m.Logf(rec.printf), rec
}
