package paths

import "testing"

func TestParseMountinfo(t *testing.T) {
	cases := []struct {
		line, mount, typ string
		ok               bool
	}{
		{"36 35 98:0 /mnt1 /mnt/parent rw,noatime master:1 - ext3 /dev/root rw", "/mnt/parent", "ext3", true},
		{"40 22 0:51 / /home/a\\040b rw - nfs4 server:/home rw", "/home/a b", "nfs4", true},
		{"41 22 0:52 / /mnt/remote rw,nosuid shared:5 - fuse.sshfs host:/ rw", "/mnt/remote", "fuse.sshfs", true},
		{"garbage", "", "", false},
	}
	for _, tc := range cases {
		mount, typ, ok := parseMountinfo(tc.line)
		if mount != tc.mount || typ != tc.typ || ok != tc.ok {
			t.Errorf("%q: got %q %q %v", tc.line, mount, typ, ok)
		}
	}
}

func TestFSTypeOfTempDir(t *testing.T) {
	if typ, err := fsType(t.TempDir()); err != nil || typ == "" {
		t.Fatalf("got %q, %v", typ, err)
	}
}
