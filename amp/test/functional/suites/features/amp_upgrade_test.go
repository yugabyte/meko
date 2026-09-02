package features

import (
	"strings"
	"testing"

	"github.com/yugabyte/amp/test/functional/framework/releases"
)

func TestPreviousRelease(t *testing.T) {
	list := []releases.Release{
		{Version: "0.260901.0", Tag: "0.260901.0-17e146896"},
		{Version: "0.260831.0", Tag: "0.260831.0-ebb2c3058"},
		{Version: "0.260830.0", Tag: "0.260830.0-cd04f8c9a"},
	}
	cases := []struct {
		name, to, want, wantErr string
	}{
		{"newest release takes the one behind it", "amp-controller:0.260901.0-17e146896", "0.260831.0-ebb2c3058", ""},
		{"older release takes the one behind it, not the newest", "amp-controller:0.260831.0-ebb2c3058", "0.260830.0-cd04f8c9a", ""},
		{"a release the portal has not seen yet takes the newest older one", "amp-controller:0.260902.0-abcdef123", "0.260901.0-17e146896", ""},
		{"a branch build takes the newest release", "amp-controller:custom-9844268af", "0.260901.0-17e146896", ""},
		{"a bare tag works too", "0.260901.0-17e146896", "0.260831.0-ebb2c3058", ""},
		{"the oldest release has nothing to upgrade from", "amp-controller:0.260830.0-cd04f8c9a", "", "none is older than 0.260830.0-cd04f8c9a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := previousRelease(list, tc.to)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Tag != tc.want {
				t.Errorf("previous of %s = %s, want %s", tc.to, got.Tag, tc.want)
			}
		})
	}
	if _, err := previousRelease(nil, "amp-controller:custom-abc1234"); err == nil {
		t.Error("an empty release list must not resolve a previous release")
	}
}
