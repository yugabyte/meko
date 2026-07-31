package yba

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// bootstrapClient builds a Client against the fake with no customer/token yet
// (the pre-bootstrap state), and a fast retry cadence so the loop test doesn't
// sleep for whole seconds.
func (f *fakeYBA) bootstrapClient() *Client {
	c := New(f.srv.URL, "", "", WithHTTPClient(f.srv.Client()))
	c.BootstrapRetry = time.Millisecond
	return c
}

func TestAppVersionProbe(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("GET /api/v1/app_version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"version": "2025.2.1.0-b141"})
	})
	v, err := f.bootstrapClient().AppVersion(context.Background())
	if err != nil {
		t.Fatalf("app_version: %v", err)
	}
	if v != "2025.2.1.0-b141" {
		t.Errorf("version = %q", v)
	}
}

func TestRegisterAlreadyRegisteredSentinel(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Cannot register multiple accounts in Single Customer Mode"})
	})
	err := f.bootstrapClient().Register(context.Background(), RegisterReq{Name: "demo", Email: "demo@yugabyte.com", Password: "pw"})
	if !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("err = %v, want ErrAlreadyRegistered", err)
	}
}

func TestRegisterSuccessCapturesSession(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authToken": "sess-1", "customerUUID": "cust-1"})
	})
	c := f.bootstrapClient()
	if err := c.Register(context.Background(), RegisterReq{Email: "demo@yugabyte.com", Password: "pw"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if c.authToken != "sess-1" || c.customerUUID != "cust-1" {
		t.Errorf("session/customer not captured: %q/%q", c.authToken, c.customerUUID)
	}
}

func TestRegisterHardErrorPropagates(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "boom"})
	})
	err := f.bootstrapClient().Register(context.Background(), RegisterReq{Email: "d@e.com", Password: "pw"})
	if err == nil || errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("err = %v, want a propagated non-sentinel error", err)
	}
}

func TestLoginCapturesTokenAndCustomer(t *testing.T) {
	f := newFake()
	defer f.close()
	f.on("POST /api/v1/login", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authToken": "sess-xyz", "customerUUID": "cust-9"})
	})
	c := f.bootstrapClient()
	cust, tok, err := c.Login(context.Background(), "demo@yugabyte.com", "pw")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if cust != "cust-9" || tok != "sess-xyz" {
		t.Errorf("returned %q/%q", cust, tok)
	}
	if c.customerUUID != "cust-9" || c.authToken != "sess-xyz" {
		t.Errorf("client not updated: %q/%q", c.customerUUID, c.authToken)
	}
}

func TestMintAPITokenUsesSessionHeader(t *testing.T) {
	f := newFake()
	defer f.close()
	var seenSession string
	f.on("PUT /api/v1/customers/cust-9/api_token", func(w http.ResponseWriter, r *http.Request) {
		seenSession = r.Header.Get(sessionHeader)
		writeJSON(w, http.StatusOK, map[string]any{"apiToken": "durable-token"})
	})
	c := f.bootstrapClient()
	c.authToken = "sess-xyz"
	tok, err := c.MintAPIToken(context.Background(), "cust-9")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "durable-token" {
		t.Errorf("token = %q", tok)
	}
	if seenSession != "sess-xyz" {
		t.Errorf("mint sent session header %q, want sess-xyz", seenSession)
	}
	if c.apiToken != "durable-token" || c.customerUUID != "cust-9" {
		t.Errorf("client not updated after mint: %q/%q", c.apiToken, c.customerUUID)
	}
}

func TestMintAPITokenRequiresSession(t *testing.T) {
	f := newFake()
	defer f.close()
	if _, err := f.bootstrapClient().MintAPIToken(context.Background(), "cust-9"); err == nil {
		t.Fatalf("expected error without a session token")
	}
}

// TestBootstrapRetryLoopReachesSuccess: app_version fails while the pod is still
// coming up, then the full register(already-registered)→login→mint sequence
// completes, and Bootstrap returns the durable token / customer / version.
func TestBootstrapRetryLoopReachesSuccess(t *testing.T) {
	f := newFake()
	defer f.close()

	var mu sync.Mutex
	versionCalls := 0
	f.on("GET /api/v1/app_version", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		versionCalls++
		n := versionCalls
		mu.Unlock()
		if n < 3 {
			http.Error(w, `{"error":"starting"}`, http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"version": "2025.2.1.0-b141"})
	})
	f.on("POST /api/v1/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "already registered"})
	})
	f.on("POST /api/v1/login", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"authToken": "sess-1", "customerUUID": "cust-boot"})
	})
	f.on("PUT /api/v1/customers/cust-boot/api_token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"apiToken": "final-token"})
	})

	c := f.bootstrapClient()
	res, err := c.Bootstrap(context.Background(), BootstrapCreds{Name: "demo", Email: "demo@yugabyte.com", Password: "pw"}, 5*time.Second)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if res.APIToken != "final-token" || res.CustomerUUID != "cust-boot" || res.Version != "2025.2.1.0-b141" {
		t.Errorf("result = %+v", res)
	}
	if c.apiToken != "final-token" {
		t.Errorf("client apiToken = %q, want final-token", c.apiToken)
	}
}

func TestBootstrapTimesOut(t *testing.T) {
	f := newFake()
	defer f.close()
	// app_version never becomes ready → Bootstrap must time out, not spin forever.
	f.on("GET /api/v1/app_version", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"starting"}`, http.StatusServiceUnavailable)
	})
	c := f.bootstrapClient()
	_, err := c.Bootstrap(context.Background(), BootstrapCreds{Email: "d@e.com", Password: "pw"}, 20*time.Millisecond)
	if err == nil {
		t.Fatalf("expected timeout error")
	}
}
