package server

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// sniffTimeout is how long a connection has to send its first byte before Sniff closes it, as long
// as http.Server gives a request's headers.
const sniffTimeout = 10 * time.Second

// tlsHandshake is a TLS record's first byte: a handshake (RFC 8446 5.1).
const tlsHandshake = 0x16

// plainFromElsewhere is the reply to plain HTTP from another computer, which is only for this one.
const plainFromElsewhere = "Meko takes plain HTTP only from this computer. Use https://.\n"

// Sniff is remote access's listener: one port for browsers anywhere over TLS and for agents and the
// CLI on this computer over plain HTTP (local MCP clients can't use a self-signed certificate). It
// reads each connection's first byte: a TLS handshake is served over TLS with cfg (an http.Server
// sees a *tls.Conn, so r.TLS is set); anything else is plain HTTP, served only from a loopback
// peer, and from anywhere else answered with a 400 and closed. The first byte is read off the
// accepting goroutine, so a slow client holds up no other.
func Sniff(ln net.Listener, cfg *tls.Config) net.Listener {
	s := &sniffer{Listener: ln, cfg: cfg, timeout: sniffTimeout, conns: make(chan net.Conn), errs: make(chan error),
		done: make(chan struct{})}
	go s.accept()
	return s
}

type sniffer struct {
	net.Listener
	cfg     *tls.Config
	timeout time.Duration // for the first byte
	conns   chan net.Conn
	errs    chan error
	done    chan struct{}
	once    sync.Once
}

func (s *sniffer) accept() {
	for {
		c, err := s.Listener.Accept()
		if err != nil {
			select {
			case s.errs <- err:
			case <-s.done:
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// A temporary failure (out of file descriptors): http.Server waits and asks again.
			continue
		}
		go s.classify(c)
	}
}

func (s *sniffer) classify(c net.Conn) {
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(s.timeout))
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return
	}
	var out net.Conn = &peekedConn{Conn: c, r: br}
	switch {
	case first[0] == tlsHandshake:
		out = tls.Server(out, s.cfg)
	case !loopbackAddr(c.RemoteAddr()):
		_ = c.SetWriteDeadline(time.Now().Add(s.timeout))
		_, _ = fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain; charset=utf-8\r\n"+
			"Connection: close\r\nContent-Length: %d\r\n\r\n%s", len(plainFromElsewhere), plainFromElsewhere)
		_ = c.Close()
		return
	}
	select {
	case s.conns <- out:
	case <-s.done:
		_ = out.Close()
	}
}

// Accept returns the next connection Sniff has sorted.
func (s *sniffer) Accept() (net.Conn, error) {
	select {
	case c := <-s.conns:
		return c, nil
	case err := <-s.errs:
		return nil, err
	case <-s.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting; connections still being sorted are closed.
func (s *sniffer) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.Listener.Close()
}

// peekedConn is a connection whose first bytes were read into r.
type peekedConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *peekedConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// loopbackAddr reports whether a connection's peer is on this computer.
func loopbackAddr(a net.Addr) bool {
	ap, err := netip.ParseAddrPort(a.String())
	return err == nil && ap.Addr().Unmap().IsLoopback()
}
