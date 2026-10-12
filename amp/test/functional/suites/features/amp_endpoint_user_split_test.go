package features

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// copyProgramConn answers computeShell's statements: it captures the COPY's
// PROGRAM and serves output as the probe table's rows.
type copyProgramConn struct {
	program string
	output  string
}

func (c *copyProgramConn) Exec(_ context.Context, sql string, _ ...any) (int64, error) {
	if rest, ok := strings.CutPrefix(sql, "COPY "+splitProbeTable+" (line) FROM PROGRAM '"); ok {
		c.program = strings.TrimSuffix(rest, "'")
	}
	return 0, nil
}

func (c *copyProgramConn) QueryInt(context.Context, string, ...any) (int64, error) { return 0, nil }

func (c *copyProgramConn) QueryString(context.Context, string, ...any) (string, error) {
	return base64.StdEncoding.EncodeToString([]byte(c.output)), nil
}

func (c *copyProgramConn) Close() {}

func TestComputeShellCarriesTheScriptIntact(t *testing.T) {
	script := "echo \"a=$(id -u)\"\nprintf '%s\\r\\n' 'it''s' >&3\n"
	c := &copyProgramConn{output: "uid=1001\nkey_rc=1\nweird=a\\b'c\n"}
	out, err := computeShell(context.Background(), c, script)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.program, "'") {
		t.Fatalf("PROGRAM %q carries a quote, which would end the SQL literal", c.program)
	}
	enc, _, ok := strings.Cut(strings.TrimPrefix(c.program, "echo "), " ")
	if !ok {
		t.Fatalf("PROGRAM %q does not start with the encoded script", c.program)
	}
	got, err := base64.StdEncoding.DecodeString(enc)
	if err != nil || string(got) != script {
		t.Fatalf("PROGRAM decodes to %q (%v), want %q", got, err, script)
	}
	if out != c.output {
		t.Fatalf("output %q, want %q", out, c.output)
	}
	if kv := parseKV(out); kv["uid"] != "1001" || kv["key_rc"] != "1" || kv["weird"] != `a\b'c` {
		t.Fatalf("parseKV = %v", kv)
	}
}
