package mysql

import "github.com/google/uuid"

// newDeletionID mints the UUID stamped on every archive row of one delete
// operation. Kept as a helper so the interactive and scheduled archive paths
// share the same audit convention.
func newDeletionID() string { return uuid.NewString() }
