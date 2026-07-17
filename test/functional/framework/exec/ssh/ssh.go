// Package ssh is the SSH remote-execution transport: it runs commands, streams
// long-running ones, and copies files (over cat-through-a-session, no sftp) on a
// VM reached by key auth. It implements exec.RemoteExecutor and adds Fetch and
// Start beyond that minimal contract.
package ssh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	execpkg "github.com/yugabyte/yb-amp/test/functional/framework/exec"
)

// MaxCapture bounds the stdout/stderr a single Run buffers in memory.
const MaxCapture = 10 << 20 // 10 MiB

// Config describes how to reach a host.
type Config struct {
	Host        string
	Port        int           // 0 → 22
	User        string        // "" → current OS user
	KeyPath     string        // private key file; ~ expanded
	DialTimeout time.Duration // 0 → 15s
	KeepAlive   time.Duration // 0 → 30s; ≤0 to disable
	// HostKey, when set, is verified against the server's key; otherwise the
	// host key is not checked (dev/test convenience — VMs are ephemeral).
	HostKey ssh.PublicKey
}

// Client is a live SSH connection.
type Client struct {
	conn      *ssh.Client
	stopKeep  chan struct{}
	keepAlive time.Duration
}

// Dial opens a connection using key auth. It honors ctx for the dial only; the
// returned Client outlives ctx.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	port := cfg.Port
	if port == 0 {
		port = 22
	}
	usr := cfg.User
	if usr == "" {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("resolve current user: %w", err)
		}
		usr = u.Username
	}
	signer, err := loadKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	hostKeyCb := ssh.InsecureIgnoreHostKey()
	if cfg.HostKey != nil {
		hostKeyCb = ssh.FixedHostKey(cfg.HostKey)
	}
	dialTimeout := cfg.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 15 * time.Second
	}
	clientCfg := &ssh.ClientConfig{
		User:            usr,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCb,
		Timeout:         dialTimeout,
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", port))
	d := net.Dialer{Timeout: dialTimeout}
	netConn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(netConn, addr, clientCfg)
	if err != nil {
		_ = netConn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", addr, err)
	}

	keepAlive := cfg.KeepAlive
	if keepAlive == 0 {
		keepAlive = 30 * time.Second
	}
	c := &Client{
		conn:      ssh.NewClient(sshConn, chans, reqs),
		keepAlive: keepAlive,
	}
	if keepAlive > 0 {
		c.stopKeep = make(chan struct{})
		go c.keepAliveLoop()
	}
	return c, nil
}

func (c *Client) keepAliveLoop() {
	t := time.NewTicker(c.keepAlive)
	defer t.Stop()
	for {
		select {
		case <-c.stopKeep:
			return
		case <-t.C:
			_, _, err := c.conn.SendRequest("keepalive@ampqa", true, nil)
			if err != nil {
				return
			}
		}
	}
}

// Close terminates the connection.
func (c *Client) Close() error {
	if c.stopKeep != nil {
		close(c.stopKeep)
		c.stopKeep = nil
	}
	return c.conn.Close()
}

// Run executes cmd, capturing bounded stdout/stderr, and returns the exit code.
// It respects ctx: on cancellation the session is closed, killing the command.
func (c *Client) Run(ctx context.Context, cmd string) (execpkg.Result, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return execpkg.Result{}, fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()

	var outBuf, errBuf capBuffer
	outBuf.limit = MaxCapture
	errBuf.limit = MaxCapture
	sess.Stdout = &outBuf
	sess.Stderr = &errBuf

	if err := sess.Start(cmd); err != nil {
		return execpkg.Result{}, fmt.Errorf("start %q: %w", cmd, err)
	}

	waitErr := waitWithCtx(ctx, sess)
	res := execpkg.Result{
		Stdout:    outBuf.String(),
		Stderr:    errBuf.String(),
		Truncated: outBuf.truncated || errBuf.truncated,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		res.ExitCode = -1
		return res, ctxErr
	}
	res.ExitCode = exitCode(waitErr)
	if waitErr != nil {
		var ee *ssh.ExitError
		if !errors.As(waitErr, &ee) {
			return res, fmt.Errorf("run %q: %w", cmd, waitErr)
		}
	}
	return res, nil
}

// Handle is a running command started by Start. Stdout/Stderr stream live.
type Handle struct {
	sess     *ssh.Session
	stdout   io.Reader
	stderr   io.Reader
	done     chan struct{} // closed once, stops the ctx watcher
	stopOnce sync.Once
}

func (h *Handle) stopWatch() { h.stopOnce.Do(func() { close(h.done) }) }

// Stdout is the command's live standard output.
func (h *Handle) Stdout() io.Reader { return h.stdout }

// Stderr is the command's live standard error.
func (h *Handle) Stderr() io.Reader { return h.stderr }

// Wait blocks until the command exits and returns its exit code.
func (h *Handle) Wait() (int, error) {
	err := h.sess.Wait()
	h.stopWatch()
	code := exitCode(err)
	_ = h.sess.Close()
	var ee *ssh.ExitError
	if err != nil && !errors.As(err, &ee) {
		return code, err
	}
	return code, nil
}

// Kill terminates the command and closes the session.
func (h *Handle) Kill() error {
	h.stopWatch()
	_ = h.sess.Signal(ssh.SIGKILL)
	return h.sess.Close()
}

// Start launches a long-running command and returns a Handle to follow it. The
// command is killed if ctx is cancelled before Wait/Kill.
func (c *Client) Start(ctx context.Context, cmd string) (*Handle, error) {
	sess, err := c.conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := sess.Start(cmd); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("start %q: %w", cmd, err)
	}
	h := &Handle{sess: sess, stdout: stdout, stderr: stderr, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close()
		case <-h.done:
		}
	}()
	return h, nil
}

// Copy uploads localPath to remotePath via a cat-into-file session and verifies
// the sha256 matches on both sides.
func (c *Client) Copy(ctx context.Context, localPath, remotePath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", localPath, err)
	}
	defer f.Close()

	want, err := fileSHA256(localPath)
	if err != nil {
		return err
	}

	sess, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	sess.Stdin = f
	if err := sess.Start(fmt.Sprintf("cat > %s", shellQuote(remotePath))); err != nil {
		return fmt.Errorf("start upload: %w", err)
	}
	if err := waitWithCtx(ctx, sess); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("upload %s → %s: %w", localPath, remotePath, err)
	}

	got, err := c.remoteSHA256(ctx, remotePath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checksum mismatch after upload of %s: local %s remote %s", remotePath, want, got)
	}
	return nil
}

// Fetch downloads remotePath to localPath via a cat-from-file session and
// verifies the sha256 matches on both sides.
func (c *Client) Fetch(ctx context.Context, remotePath, localPath string) error {
	if dir := filepath.Dir(localPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	f, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", localPath, err)
	}
	defer f.Close()

	sess, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("new session: %w", err)
	}
	defer sess.Close()
	sess.Stdout = f
	if err := sess.Start(fmt.Sprintf("cat %s", shellQuote(remotePath))); err != nil {
		return fmt.Errorf("start download: %w", err)
	}
	if err := waitWithCtx(ctx, sess); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("download %s → %s: %w", remotePath, localPath, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", localPath, err)
	}

	want, err := c.remoteSHA256(ctx, remotePath)
	if err != nil {
		return err
	}
	got, err := fileSHA256(localPath)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("checksum mismatch after download of %s: remote %s local %s", remotePath, want, got)
	}
	return nil
}

func (c *Client) remoteSHA256(ctx context.Context, path string) (string, error) {
	res, err := c.Run(ctx, fmt.Sprintf("sha256sum %s", shellQuote(path)))
	if err != nil {
		return "", fmt.Errorf("remote sha256 %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("remote sha256 %s exited %d: %s", path, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	fields := strings.Fields(res.Stdout)
	if len(fields) == 0 {
		return "", fmt.Errorf("remote sha256 %s: empty output", path)
	}
	return fields[0], nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// waitWithCtx waits for sess to finish, closing it (killing the command) if ctx
// is cancelled first.
func waitWithCtx(ctx context.Context, sess *ssh.Session) error {
	done := make(chan error, 1)
	go func() { done <- sess.Wait() }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		<-done
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// exitCode extracts the exit code from a session Wait error. A clean exit is 0;
// a non-zero exit yields its status; a signal or other error yields -1.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *ssh.ExitError
	if errors.As(err, &ee) {
		return ee.ExitStatus()
	}
	return -1
}

func loadKey(keyPath string) (ssh.Signer, error) {
	if keyPath == "" {
		return nil, fmt.Errorf("ssh: no private key path configured")
	}
	expanded, err := expandUser(keyPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(expanded)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", expanded, err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", expanded, err)
	}
	return signer, nil
}

// expandUser resolves a leading ~ or ~/ to the current user's home directory.
func expandUser(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, path[2:]), nil
}

// shellQuote wraps s in single quotes, escaping embedded single quotes, so it is
// safe as one argument in a remote /bin/sh command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// capBuffer is a bytes.Buffer capped at limit; once full it records truncation
// and drops further writes.
type capBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *capBuffer) String() string { return b.buf.String() }
