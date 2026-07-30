package api

import "testing"

func TestNormalizeBasePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"/", ""},
		{"//", ""},
		{"   ", ""},
		{"ycp", "/ycp"},
		{"/ycp", "/ycp"},
		{"/ycp/", "/ycp"},
		{"ycp/", "/ycp"},
		{"//ycp//", "/ycp"},
		{"  /ycp/  ", "/ycp"},
		{"meko/ycp", "/meko/ycp"},
		{"/meko//ycp/", "/meko/ycp"},
		{"/ycp-v2_1.0", "/ycp-v2_1.0"},
	}
	for _, c := range cases {
		got, err := normalizeBasePath(c.in)
		if err != nil {
			t.Errorf("normalizeBasePath(%q) returned error %v, want %q", c.in, err, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormalizeBasePathRejects covers the values that must not boot. A full URL
// is the one that matters most: it is the likely operator typo (the agent's
// YCP_URL for the same deployment really is a full URL), and coercing it would
// mount every route under "/https:/devcloud.yugabyte.com/ycp" while the health
// probe kept the pod Ready.
func TestNormalizeBasePathRejects(t *testing.T) {
	for _, in := range []string{
		"https://devcloud.yugabyte.com/ycp",
		"devcloud.yugabyte.com:443/ycp",
		"/ycp/*",
		"/ycp/:id",
		"/../ycp",
		"/ycp/..",
		"/ycp/./x",
		"/ycp%2fapi",
		"/ycp?a=b",
		"/ycp#frag",
		"/my ycp",
	} {
		got, err := normalizeBasePath(in)
		if err == nil {
			t.Errorf("normalizeBasePath(%q) = %q, want an error", in, got)
		}
	}
}
