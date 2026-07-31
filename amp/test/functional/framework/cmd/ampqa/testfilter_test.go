package main

import "testing"

func TestResolveTestFilter(t *testing.T) {
	cases := []struct {
		name, exact, regex, want string
		wantErr                  bool
	}{
		{name: "exact name → anchored + escaped", exact: "perf/pgbench", want: `^perf/pgbench$`},
		{name: "exact escapes regex metacharacters", exact: "a.b+c(x)", want: `^a\.b\+c\(x\)$`},
		{name: "regex passes through raw", regex: `^canary/(pass|fail)$`, want: `^canary/(pass|fail)$`},
		{name: "neither → match all", want: ""},
		{name: "both → error", exact: "x", regex: "y", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveTestFilter(c.exact, c.regex)
			if c.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
