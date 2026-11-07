package server

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yugabyte/meko/src/meko/internal/tlscert"
)

// One port serves TLS and plain HTTP from this computer, each as itself: the handler sees r.TLS on
// a TLS connection only.
func TestSniffServesTLSAndPlain(t *testing.T) {
	pair, err := tlscert.SelfSigned(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sl := Sniff(ln, &tls.Config{Certificates: []tls.Certificate{pair.Cert}, MinVersion: tls.VersionTLS12})
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, map[bool]string{true: "tls", false: "plain"}[r.TLS != nil])
		})}
	go func() { _ = srv.Serve(sl) }()
	t.Cleanup(func() { _ = srv.Close() })
	host := ln.Addr().String()
	for scheme, want := range map[string]string{"http": "plain", "https": "tls"} {
		resp, gerr := testClient.Get(scheme + "://" + host + "/")
		if gerr != nil {
			t.Fatalf("%s: %v", scheme, gerr)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(b) != want {
			t.Errorf("%s: %q, want %q", scheme, b, want)
		}
	}
	// A connection that hasn't sent its first byte holds up no other.
	idle, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = idle.Close() }()
	if resp, err := testClient.Get("http://" + host + "/"); err != nil {
		t.Fatalf("beside an idle connection: %v", err)
	} else {
		_ = resp.Body.Close()
	}
}

// Plain HTTP from another computer gets a 400 that says to use https://, and is closed.
func TestSniffRefusesPlainFromElsewhere(t *testing.T) {
	s := &sniffer{timeout: 5 * time.Second, conns: make(chan net.Conn, 1), done: make(chan struct{})}
	server, client := net.Pipe()
	go s.classify(&remoteConn{Conn: server})
	go func() { _, _ = io.WriteString(client, "GET / HTTP/1.1\r\nHost: box:7777\r\n\r\n") }()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "https://") {
		t.Errorf("%d %q", resp.StatusCode, b)
	}
	select {
	case c := <-s.conns:
		t.Errorf("served %v", c.RemoteAddr())
	default:
	}
}

// A connection that sends nothing is closed at the sniffer's timeout.
func TestSniffClosesASilentConnection(t *testing.T) {
	s := &sniffer{timeout: 50 * time.Millisecond, conns: make(chan net.Conn, 1), done: make(chan struct{})}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.classify(&remoteConn{Conn: server})
	}()
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err != io.EOF {
		t.Errorf("read: %v, want EOF", err)
	}
	<-done
}

// remoteConn is a connection from another computer.
type remoteConn struct{ net.Conn }

func (remoteConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.7"), Port: 50000}
}
