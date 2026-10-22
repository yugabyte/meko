package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// KeychainService is the service name of every item Meko keeps in the Keychain. Each item's
// account is <data folder>/<name>, so two data folders never share a secret.
const KeychainService = "com.yugabyte.meko"

const defaultSecurity = "/usr/bin/security"

// securityPath is Apple's security tool; tests point it at a fake.
var securityPath = defaultSecurity

// keychainTimeout bounds one call to security, which can wait on a locked Keychain.
const keychainTimeout = 15 * time.Second

// Keychain keeps secrets as generic passwords in the login Keychain, through /usr/bin/security. Each
// item's access list then names that Apple-signed tool, which never changes, so an updated
// (unsigned) meko never meets a Keychain prompt. The value is written through security's command
// reader on stdin (security -i), never as an argument another process could see.
type Keychain struct {
	root string
	// keychain is a Keychain file to use instead of the default one; only tests set it.
	keychain string
}

// NewKeychain is the Keychain store of the data folder at root.
func NewKeychain(root string) *Keychain { return &Keychain{root: root} }

// Kind is KindKeychain.
func (k *Keychain) Kind() string { return KindKeychain }

func (k *Keychain) account(name string) string { return k.root + "/" + name }

// errItemNotFound is security's exit status for a missing item.
const errItemNotFound = 44

// Get reads the secret. A locked Keychain (no login session, as over SSH) is an error saying so.
func (k *Keychain) Get(name string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	args := []string{"find-generic-password", "-s", KeychainService, "-a", k.account(name), "-w"}
	out, err := k.run(nil, k.withKeychain(args)...)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == errItemNotFound {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Set adds or replaces the secret, then reads it back: security -i doesn't report a failed
// command in its exit status.
func (k *Keychain) Set(name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if err := checkValue(value); err != nil {
		return err
	}
	acct := k.account(name)
	if strings.ContainsAny(acct, "\"\\\n") {
		return fmt.Errorf("the data folder %s can't name a Keychain item: it holds a quote, backslash or newline", k.root)
	}
	line := fmt.Sprintf("add-generic-password -U -s %q -a %q -w %q", KeychainService, acct, value)
	if k.keychain != "" {
		line += fmt.Sprintf(" %q", k.keychain)
	}
	if _, err := k.run(strings.NewReader(line+"\n"), "-i"); err != nil {
		return err
	}
	got, err := k.Get(name)
	if err != nil {
		return err
	}
	if got != value {
		return fmt.Errorf("the Keychain didn't keep %s (is it locked?)", name)
	}
	return nil
}

// Add stores the secret unless the store already has one by that name, and returns whichever it
// holds after: without -U, add-generic-password refuses an existing item, so two callers adding at
// once agree on the first one's value.
func (k *Keychain) Add(name, value string) (string, error) {
	if err := checkName(name); err != nil {
		return "", err
	}
	if err := checkValue(value); err != nil {
		return "", err
	}
	acct := k.account(name)
	if strings.ContainsAny(acct, "\"\\\n") {
		return "", fmt.Errorf("the data folder %s can't name a Keychain item: it holds a quote, backslash or newline", k.root)
	}
	line := fmt.Sprintf("add-generic-password -s %q -a %q -w %q", KeychainService, acct, value)
	if k.keychain != "" {
		line += fmt.Sprintf(" %q", k.keychain)
	}
	// An item that's already there is reported as an error on stderr; the Get below settles it.
	_, _ = k.run(strings.NewReader(line+"\n"), "-i")
	return k.Get(name)
}

// Delete removes the secret.
func (k *Keychain) Delete(name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	args := []string{"delete-generic-password", "-s", KeychainService, "-a", k.account(name)}
	_, err := k.run(nil, k.withKeychain(args)...)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == errItemNotFound {
		return nil
	}
	return err
}

func (k *Keychain) withKeychain(args []string) []string {
	if k.keychain != "" {
		return append(args, k.keychain)
	}
	return args
}

// run runs security; its error carries security's own message (which never holds the secret) but
// not its output.
func (k *Keychain) run(stdin *strings.Reader, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityPath, args...) //nolint:gosec // G204: Apple's security tool
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == errItemNotFound {
			return "", err
		}
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &keychainError{op: args[0], msg: msg, err: err}
	}
	if stdin != nil && strings.TrimSpace(errb.String()) != "" {
		// security -i prints a failed command's error and still exits 0.
		return "", &keychainError{op: "add-generic-password", msg: strings.TrimSpace(errb.String())}
	}
	return out.String(), nil
}

type keychainError struct {
	op, msg string
	err     error
}

func (e *keychainError) Error() string {
	return fmt.Sprintf("the Keychain refused %s: %s. If it's locked (as over SSH), unlock it with "+
		"security unlock-keychain, or set [secrets] store = %q in config.toml", e.op, e.msg, KindFile)
}

func (e *keychainError) Unwrap() error { return e.err }
