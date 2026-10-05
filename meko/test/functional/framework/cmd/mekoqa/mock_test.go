package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func runMock(t *testing.T, args ...string) (stop func()) {
	t.Helper()
	root := buildRoot()
	root.SetArgs(append([]string{"mock"}, args...))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("mock %v: %v", args, err)
		}
	}
}

func waitGet(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not answer: %v", url, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMockLLMCommandServesTheControlAPI(t *testing.T) {
	port := freePort(t)
	defer runMock(t, "llm", "--host", "127.0.0.1", "--port", fmt.Sprint(port))()
	waitGet(t, fmt.Sprintf("http://127.0.0.1:%d/_mock/health", port))
}

func TestMockCognitoCommandServesWithoutTLSOrAKey(t *testing.T) {
	tlsPort, ctlPort := freePort(t), freePort(t)
	defer runMock(t, "cognito", "--host", "127.0.0.1", "--tls-port", fmt.Sprint(tlsPort), "--ctl-port", fmt.Sprint(ctlPort))()
	waitGet(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", ctlPort))
	waitGet(t, fmt.Sprintf("http://127.0.0.1:%d/us-east-1_MekoQa001/.well-known/jwks.json", tlsPort))
}
