package scheduledcontract

import "errors"

// ErrRevisionConflict is the shared optimistic-concurrency conflict for task
// configuration updates. The Scheduler store and service both return it; the
// RPC layer maps it to the stable 409 code.
var ErrRevisionConflict = errors.New("scheduled task revision conflict")
