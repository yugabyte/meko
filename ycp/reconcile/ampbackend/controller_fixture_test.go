package ampbackend

import (
	"errors"
	"testing"

	"meko/ycp/reconcile"
)

func mustPending(t *testing.T, err error) {
	t.Helper()
	var pending *reconcile.PendingError
	if !errors.As(err, &pending) {
		t.Fatalf("got %v (%T), want a PendingError — the upgrade is in flight, neither applied nor failed", err, err)
	}
}

// ctlRow reads the controller row out of a List, and whether it was emitted at
// all. A row is emitted only for an environment that both declares the kind and
// runs an agent configured to converge it.
func ctlRow(t *testing.T, b *Backend) (reconcile.Actual, bool) {
	t.Helper()
	actuals, err := b.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range actuals {
		if a.Key.Kind == KindController {
			return a, true
		}
	}
	return reconcile.Actual{}, false
}
