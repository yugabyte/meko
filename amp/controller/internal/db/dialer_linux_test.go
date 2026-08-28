//go:build linux

package db

import (
	"context"
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDialerSetsTCPUserTimeout reads the option back off a real socket rather
// than asserting the constant, because the failure mode this guards is a silent
// one: a Control hook that never runs, or a setsockopt that errors and is
// swallowed, leaves connections that look fine and then hang for ~15 minutes
// when the peer disappears (#961). Only the kernel's own answer proves it stuck.
func TestDialerSetsTCPUserTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()

	conn, err := dialFuncWithTimeout(connectTimeout)(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if c, ok := <-accepted; ok {
		defer c.Close()
	}

	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		t.Fatalf("dialer returned %T, want *net.TCPConn", conn)
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}

	var got int
	var optErr error
	if err := raw.Control(func(fd uintptr) {
		got, optErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_USER_TIMEOUT)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if optErr != nil {
		t.Fatalf("getsockopt TCP_USER_TIMEOUT: %v", optErr)
	}
	if want := int(tcpUserTimeout.Milliseconds()); got != want {
		t.Errorf("TCP_USER_TIMEOUT = %dms, want %dms", got, want)
	}
}
