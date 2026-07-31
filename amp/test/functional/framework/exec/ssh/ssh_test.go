package ssh_test

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	sshx "github.com/yugabyte/amp/test/functional/framework/exec/ssh"
)

// testServer is an in-process SSH server double: it authenticates the test's
// key and executes each exec request through the local /bin/sh. It is NOT a
// real environment — it exists only so the transport can be exercised over a
// loopback connection.
type testServer struct {
	ln         net.Listener
	hostSigner ssh.Signer
	authKey    ssh.PublicKey
	wg         sync.WaitGroup
}

func newTestServer(t *testing.T) (*testServer, sshx.Config) {
	t.Helper()

	// Host key.
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen host key: %v", err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatalf("host signer: %v", err)
	}

	// Client key, written to a temp PEM file for the transport to load.
	_, clientPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen client key: %v", err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPriv)
	if err != nil {
		t.Fatalf("client signer: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(clientPriv, "")
	if err != nil {
		t.Fatalf("marshal client key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write client key: %v", err)
	}

	s := &testServer{hostSigner: hostSigner, authKey: clientSigner.PublicKey()}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.ln = ln
	s.wg.Add(1)
	go s.serve()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	portNum, _ := strconv.Atoi(port)
	cfg := sshx.Config{
		Host:      host,
		Port:      portNum,
		User:      "tester",
		KeyPath:   keyPath,
		HostKey:   hostSigner.PublicKey(),
		KeepAlive: -1,
	}
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s, cfg
}

func (s *testServer) serverConfig() *ssh.ServerConfig {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(s.authKey.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized key")
		},
	}
	cfg.AddHostKey(s.hostSigner)
	return cfg
}

func (s *testServer) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go s.handleConn(conn)
	}
}

func (s *testServer) handleConn(conn net.Conn) {
	defer s.wg.Done()
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, s.serverConfig())
	if err != nil {
		return
	}
	defer sshConn.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, chReqs)
	}
}

func handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	var cmd *exec.Cmd
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			_ = ssh.Unmarshal(req.Payload, &payload)
			_ = req.Reply(true, nil)
			cmd = exec.Command("/bin/sh", "-c", payload.Command)
			stdin, _ := cmd.StdinPipe()
			cmd.Stdout = ch
			cmd.Stderr = ch.Stderr()
			if err := cmd.Start(); err != nil {
				sendExit(ch, 127)
				_ = ch.Close()
				continue
			}
			go func() { _, _ = io.Copy(stdin, ch); _ = stdin.Close() }()
			go func(c *exec.Cmd) {
				werr := c.Wait()
				code := 0
				var ee *exec.ExitError
				if errors.As(werr, &ee) {
					code = ee.ExitCode()
				} else if werr != nil {
					code = 255
				}
				sendExit(ch, code)
				_ = ch.Close()
			}(cmd)
		case "signal":
			if cmd != nil && cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func sendExit(ch ssh.Channel, code int) {
	msg := struct{ Status uint32 }{uint32(code)}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(&msg))
}

func TestRunExitCodesAndOutput(t *testing.T) {
	_, cfg := newTestServer(t)
	c, err := sshx.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	res, err := c.Run(context.Background(), "printf hello; printf oops 1>&2")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d, want 0", res.ExitCode)
	}
	if res.Stdout != "hello" {
		t.Errorf("stdout = %q, want hello", res.Stdout)
	}
	if res.Stderr != "oops" {
		t.Errorf("stderr = %q, want oops", res.Stderr)
	}

	res, err = c.Run(context.Background(), "exit 7")
	if err != nil {
		t.Fatalf("run exit 7: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", res.ExitCode)
	}
}

func TestRunContextCancel(t *testing.T) {
	_, cfg := newTestServer(t)
	c, err := sshx.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = c.Run(ctx, "sleep 10")
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("cancel took too long: %s", time.Since(start))
	}
}

func TestRunOutputCap(t *testing.T) {
	_, cfg := newTestServer(t)
	c, err := sshx.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Emit more than the cap; head -c avoids depending on yes buffering.
	res, err := c.Run(context.Background(), "tr '\\0' 'a' < /dev/zero | head -c 20000000")
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Truncated {
		t.Errorf("expected truncated output")
	}
	if len(res.Stdout) > sshx.MaxCapture {
		t.Errorf("stdout %d exceeds cap %d", len(res.Stdout), sshx.MaxCapture)
	}
}

func TestStartStreaming(t *testing.T) {
	_, cfg := newTestServer(t)
	c, err := sshx.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	h, err := c.Start(context.Background(), "for i in 1 2 3; do echo line$i; done")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	var lines []string
	sc := bufio.NewScanner(h.Stdout())
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	code, err := h.Wait()
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if strings.Join(lines, ",") != "line1,line2,line3" {
		t.Errorf("streamed lines = %v", lines)
	}
}

func TestCopyFetchRoundTrip(t *testing.T) {
	_, cfg := newTestServer(t)
	c, err := sshx.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	dir := t.TempDir()
	local := filepath.Join(dir, "local.bin")
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if err := os.WriteFile(local, payload, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	remote := filepath.Join(dir, "remote.bin")
	if err := c.Copy(context.Background(), local, remote); err != nil {
		t.Fatalf("copy: %v", err)
	}
	back := filepath.Join(dir, "back.bin")
	if err := c.Fetch(context.Background(), remote, back); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	got, err := os.ReadFile(back)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sha256hex(got) != sha256hex(payload) {
		t.Errorf("round-trip checksum mismatch")
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
