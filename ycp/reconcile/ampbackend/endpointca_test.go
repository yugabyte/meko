package ampbackend

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

const testCAPEM = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"

func endpointRowAttrs(t *testing.T, b *Backend) map[string]string {
	t.Helper()
	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range actuals {
		if a.Key.Kind == KindEndpoint {
			return a.Attrs
		}
	}
	t.Fatal("List returned no endpoint")
	return nil
}

// fakeClock pins b's clock to now and returns a way to move it, so cache ages are
// measured from the read start() already made.
func fakeClock(b *Backend) func(time.Duration) {
	clock := time.Now()
	b.now = func() time.Time { return clock }
	return func(d time.Duration) { clock = clock.Add(d) }
}

// The console builds the DocumentDB connection string and the CA download from
// these attrs alone, exactly as amp_ui's endpointTls.ts does from GET …/ca.
func TestListReportsEndpointCA(t *testing.T) {
	for name, tc := range map[string]struct {
		documentdb string
		ca         string
		want       map[string]string // "" value: key must be absent
	}{
		"private CA, verifiable gateway": {
			documentdb: "ep.host:10260",
			ca:         `{"required":true,"ca_crt":` + strconv.Quote(testCAPEM) + `,"verify_name":"ep.host","documentdb_verifiable":true}`,
			want: map[string]string{"documentdb_address": "ep.host:10260", "tls_ca_required": "true",
				"tls_ca_crt": testCAPEM, "documentdb_verifiable": "true"},
		},
		"publicly trusted: no CA to hand over": {
			documentdb: "ep.host:10260",
			ca:         `{"required":false,"verify_name":"ep.host","documentdb_verifiable":true}`,
			want: map[string]string{"documentdb_address": "ep.host:10260", "tls_ca_required": "false",
				"tls_ca_crt": "", "documentdb_verifiable": "true"},
		},
		"gateway self-signs": {
			documentdb: "ep.host:10260",
			ca:         `{"required":true,"ca_crt":` + strconv.Quote(testCAPEM) + `,"verify_name":"ep.host","documentdb_verifiable":false}`,
			want:       map[string]string{"tls_ca_required": "true", "tls_ca_crt": testCAPEM, "documentdb_verifiable": "false"},
		},
		"no documentdb": {
			ca: `{"required":true,"ca_crt":` + strconv.Quote(testCAPEM) + `,"verify_name":"ep.host"}`,
			want: map[string]string{"documentdb_address": "", "tls_ca_required": "true",
				"tls_ca_crt": testCAPEM, "documentdb_verifiable": ""},
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := &genServer{endpointDocumentdb: tc.documentdb, caBody: tc.ca}
			attrs := endpointRowAttrs(t, g.start(t))
			assertAttrs(t, attrs, tc.want)
		})
	}
}

// Absent means unknown. A read that fails must not become "false", which the
// console would turn into a connection string that skips no verification it
// should — and documentdb_address, read from the endpoint row, is unaffected.
func TestFailedCAReadReportsNothing(t *testing.T) {
	g := &genServer{endpointDocumentdb: "ep.host:10260", caStatus: http.StatusInternalServerError,
		caBody: `{"error":{"code":"internal","message":"secret read failed"}}`}
	attrs := endpointRowAttrs(t, g.start(t))
	assertAttrs(t, attrs, map[string]string{"documentdb_address": "ep.host:10260",
		"tls_ca_required": "", "tls_ca_crt": "", "documentdb_verifiable": ""})
}

// Each read costs the controller two Kubernetes GETs, so it is not made every tick.
func TestCAReadIsCached(t *testing.T) {
	g := &genServer{}
	b := g.start(t)
	advance := fakeClock(b)
	endpointRowAttrs(t, b)
	if g.caGets != 1 {
		t.Fatalf("CA reads after two ticks = %d, want 1", g.caGets)
	}
	advance(caRefreshAfter)
	endpointRowAttrs(t, b)
	if g.caGets != 2 {
		t.Fatalf("CA reads past the refresh interval = %d, want 2", g.caGets)
	}
}

// Adding the documentdb extension makes documentdb_address appear at an unchanged
// address. A held answer predates it and carries no documentdb_verifiable, so it
// must be re-read at once rather than at the next refresh.
func TestCAReReadWhenDocumentdbAppears(t *testing.T) {
	g := &genServer{}
	b := g.start(t)
	fakeClock(b)
	g.endpointDocumentdb = "ep.host:10260"
	g.caBody = `{"required":false,"verify_name":"ep.host","documentdb_verifiable":true}`
	attrs := endpointRowAttrs(t, b)
	if g.caGets != 2 {
		t.Fatalf("CA reads = %d, want a re-read on the new documentdb_address", g.caGets)
	}
	assertAttrs(t, attrs, map[string]string{"documentdb_verifiable": "true"})
}

// One failed refresh must not flip the console to an unverified string: the held
// answer is repeated until it is too old to vouch for.
func TestFailedCARefreshHoldsUntilStale(t *testing.T) {
	g := &genServer{caBody: `{"required":true,"ca_crt":` + strconv.Quote(testCAPEM) + `,"verify_name":"ep.host"}`}
	b := g.start(t)
	advance := fakeClock(b)
	g.caStatus = http.StatusInternalServerError

	advance(caRefreshAfter)
	held := map[string]string{"tls_ca_required": "true", "tls_ca_crt": testCAPEM}
	assertAttrs(t, endpointRowAttrs(t, b), held)
	if g.caGets != 2 {
		t.Fatalf("CA reads = %d, want the refresh attempted", g.caGets)
	}

	advance(caStaleAfter - caRefreshAfter)
	assertAttrs(t, endpointRowAttrs(t, b), map[string]string{"tls_ca_required": "", "tls_ca_crt": ""})
}

// A failed read is retried sooner than a good one is refreshed, but not on every tick.
func TestFailedCAReadRetriesAfterBackoff(t *testing.T) {
	g := &genServer{caStatus: http.StatusInternalServerError}
	b := g.start(t)
	advance := fakeClock(b)
	g.caStatus = 0

	endpointRowAttrs(t, b)
	if g.caGets != 1 {
		t.Fatalf("CA reads inside the retry backoff = %d, want 1", g.caGets)
	}
	advance(caRetryAfter)
	assertAttrs(t, endpointRowAttrs(t, b), map[string]string{"tls_ca_required": "false"})
}

// assertAttrs checks each want key; an empty want value means the key is absent.
func assertAttrs(t *testing.T, attrs, want map[string]string) {
	t.Helper()
	for k, w := range want {
		got, ok := attrs[k]
		switch {
		case w == "" && ok:
			t.Errorf("%s = %q, want absent", k, got)
		case w != "" && got != w:
			t.Errorf("%s = %q (present %v), want %q", k, got, ok, w)
		}
	}
}

// The documentdb half of the answer is re-read when the gateway becomes reachable,
// but the CA half does not depend on it: a failed re-read must not cost the
// endpoint its CA attrs, nor carry over a documentdb_verifiable from before.
func TestDocumentdbChangeKeepsTheCAThroughAFailedRead(t *testing.T) {
	g := &genServer{caBody: `{"required":true,"ca_crt":` + strconv.Quote(testCAPEM) + `,"verify_name":"ep.host"}`}
	b := g.start(t)
	fakeClock(b)
	g.endpointDocumentdb = "ep.host:10260"
	g.caStatus = http.StatusInternalServerError
	assertAttrs(t, endpointRowAttrs(t, b), map[string]string{"documentdb_address": "ep.host:10260",
		"tls_ca_required": "true", "tls_ca_crt": testCAPEM, "documentdb_verifiable": ""})
	if g.caGets != 2 {
		t.Fatalf("CA reads = %d, want a re-read on the new documentdb_address", g.caGets)
	}
}

// A controller older than GET …/ca 404s it for every endpoint; retrying that each
// minute is load for nothing.
func TestCARouteMissingBacksOffToTheRefreshInterval(t *testing.T) {
	g := &genServer{caStatus: http.StatusNotFound, caBody: `{"error":{"code":"not_found","message":"no such route"}}`}
	b := g.start(t)
	advance := fakeClock(b)
	advance(caRetryAfter)
	assertAttrs(t, endpointRowAttrs(t, b), map[string]string{"tls_ca_required": ""})
	if g.caGets != 1 {
		t.Fatalf("CA reads one retry interval after a 404 = %d, want 1", g.caGets)
	}
	advance(caRefreshAfter - caRetryAfter)
	endpointRowAttrs(t, b)
	if g.caGets != 2 {
		t.Fatalf("CA reads one refresh interval after a 404 = %d, want 2", g.caGets)
	}
}
