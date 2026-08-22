package features

import "testing"

// pending_gc matches a relnode as 4 big-endian bytes; a relnode above 2^31
// must not sign-extend.
func TestPendingGCKeyHex(t *testing.T) {
	for _, tc := range []struct {
		relnode int64
		want    string
	}{
		{0x1234, "00001234"},
		{16385, "00004001"},
		{0xFFFFFFFF, "ffffffff"},
	} {
		if got := pendingGCKeyHex(tc.relnode); got != tc.want {
			t.Errorf("pendingGCKeyHex(%d) = %q, want %q", tc.relnode, got, tc.want)
		}
	}
}
