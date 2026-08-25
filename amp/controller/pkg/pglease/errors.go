package pglease

import (
	"errors"
	"fmt"
	"time"
)

// ErrNotAcquired reports that the lease is held by a different owner. It is the
// sentinel behind *Denial; match it with errors.Is.
var ErrNotAcquired = errors.New("pglease: lease held by another owner")

// ErrLost reports that a renewal was refused because another owner took the
// lease over. It is the cause on Held.Context() in that case.
var ErrLost = errors.New("pglease: lease taken over by another owner")

// ErrLeaseGap reports that a renewal was granted, but the database stamped it
// at or after the expiry this process already held: the lease had lapsed, so
// ownership was not continuous and another owner could have held it in between.
var ErrLeaseGap = errors.New("pglease: lease re-granted after a gap in ownership")

// ErrAlreadyHeld reports that this Client already holds the key. Two Held over
// one row would each believe they own it, and either one's Release would
// un-fence the other.
var ErrAlreadyHeld = errors.New("pglease: key already held by this client")

// ErrRenewalDeadline reports that no renewal was confirmed before
// TTL-SafetyMargin elapsed. Nobody said the lease was lost; it can simply no
// longer be proven held, which is the same thing to a writer.
var ErrRenewalDeadline = errors.New("pglease: renewal deadline passed with no confirmed renewal")

// ErrConfig reports an invalid Config or StoreConfig.
var ErrConfig = errors.New("pglease: invalid configuration")

// Denial is the error a refused Client.Acquire returns. It unwraps to
// ErrNotAcquired and carries the holder and wait the statement reported; see
// Grant for what they are worth.
type Denial struct {
	Key        string
	Holder     string
	RetryAfter time.Duration
}

func (d *Denial) Error() string {
	holder := d.Holder
	if holder == "" {
		holder = "an owner not visible at our snapshot"
	}
	return fmt.Sprintf("pglease: lease %q held by %s, may take over in %s", d.Key, holder, d.RetryAfter)
}

func (d *Denial) Unwrap() error { return ErrNotAcquired }
