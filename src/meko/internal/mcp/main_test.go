package mcp

import (
	"os"
	"testing"

	"github.com/yugabyte/meko/src/meko/internal/callhome/callhometest"
)

// TestMain turns away any report to YugabyteDB's call home service (callhome.DefaultURL) and fails
// the run if a test sent one.
func TestMain(m *testing.M) { os.Exit(callhometest.Run(m)) }
