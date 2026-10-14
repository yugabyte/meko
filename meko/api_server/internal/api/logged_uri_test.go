package api

import "testing"

// A share-link token lets anyone join, so it must never reach the access log,
// whether or not the request matched a share route.
func TestLoggedURI(t *testing.T) {
	for _, tc := range []struct{ uri, want string }{
		{"/api/v1/share/k7Qx9mP2vT8nR4wL", "/api/v1/share/:token"},
		{"/api/v1/share/k7Qx9mP2vT8nR4wL/redeem", "/api/v1/share/:token/redeem"},
		{"/api/v1/share/k7Qx9mP2vT8nR4wL/", "/api/v1/share/:token/"},
		{"/api/v1/share/k7Qx9mP2vT8nR4wL?x=1", "/api/v1/share/:token?x=1"},
		{"/share/k7Qx9mP2vT8nR4wL", "/share/:token"},
		{"/api/v1/datapacks/dp-1/share-link", "/api/v1/datapacks/dp-1/share-link"},
		{"/api/v1/datapacks/dp-1?x=1", "/api/v1/datapacks/dp-1?x=1"},
	} {
		if got := loggedURI(tc.uri); got != tc.want {
			t.Errorf("loggedURI(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
}
