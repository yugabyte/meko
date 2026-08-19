package api

import (
	"strings"
	"testing"
)

// The whole API rests on nil meaning "leave it" and `{}` meaning "clear it".
// Collapse them and an omitted field silently blanks a surface the caller has
// never heard of, which is the failure mode a partial update exists to avoid.
func TestDaemonSettingsOfKeepsNilAndEmptyApart(t *testing.T) {
	if got := daemonSettingsOf[DaemonSettings](nil); got != nil {
		t.Fatalf("nil must stay nil, got %v", got)
	}
	empty := DaemonSettings{}
	got := daemonSettingsOf(&empty)
	if got == nil {
		t.Fatal("an empty map must survive as an empty map, not become nil")
	}
	if len(*got) != 0 {
		t.Fatalf("want empty, got %v", *got)
	}
	set := DaemonSettings{"a": 1}
	if got := daemonSettingsOf(&set); got == nil || (*got)["a"] != 1 {
		t.Fatalf("value lost: %v", got)
	}
}

// A lease whose renewal interval is not under its TTL expires before anything
// renews it — the endpoint fencing itself out on a timer. Rejected here as well
// as in the supervisor, because refusing there means one endpoint logs a warning
// nobody reads, while refusing here tells the caller before the row exists that
// every endpoint on the branch will read.
func TestValidateWriteTokenSettings(t *testing.T) {
	ok := func(m WriteTokenSettings) {
		t.Helper()
		if err := validateWriteTokenSettings(&m); err != nil {
			t.Fatalf("rejected %v: %v", m, err)
		}
	}
	bad := func(m WriteTokenSettings, want string) {
		t.Helper()
		err := validateWriteTokenSettings(&m)
		if err == nil {
			t.Fatalf("accepted %v", m)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error for %v was %q, want it to mention %q", m, err, want)
		}
	}

	if err := validateWriteTokenSettings(nil); err != nil {
		t.Fatalf("an absent surface is not a change: %v", err)
	}
	ok(WriteTokenSettings{})
	ok(WriteTokenSettings{"ttl_ms": float64(15000), "renew_seconds": float64(5)})
	// One without the other is legal: the supervisor checks it against what is
	// stored, which is the only place that knows what the other currently is.
	ok(WriteTokenSettings{"ttl_ms": float64(15000)})
	ok(WriteTokenSettings{"renew_seconds": float64(5)})

	bad(WriteTokenSettings{"ttl_ms": float64(10000), "renew_seconds": float64(10)}, "must be under")
	bad(WriteTokenSettings{"ttl_ms": float64(10000), "renew_seconds": float64(11)}, "must be under")
	bad(WriteTokenSettings{"ttl_ms": float64(0), "renew_seconds": float64(5)}, "positive whole number")
	bad(WriteTokenSettings{"ttl_ms": float64(1.5)}, "positive whole number")
	bad(WriteTokenSettings{"ttl_ms": "15000"}, "must be a number")
	// A typo must not be stored and then silently ignored by the supervisor.
	bad(WriteTokenSettings{"ttl_millis": float64(15000)}, "unknown key")
}
