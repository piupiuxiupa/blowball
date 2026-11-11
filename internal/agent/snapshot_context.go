package agent

import "context"

// snapshotStoreKey carries a per-turn sub-agent transcript store. The turn
// executor injects it when the turn persists into a non-default table group
// (scheduled task conversations); the orchestrator otherwise uses the store it
// was constructed with.
type snapshotStoreKey struct{}

// WithSubAgentSnapshotStore overrides the sub-agent snapshot store for one
// turn. A nil store leaves the context unchanged.
func WithSubAgentSnapshotStore(ctx context.Context, store SubAgentSnapshotStore) context.Context {
	if store == nil {
		return ctx
	}
	return context.WithValue(ctx, snapshotStoreKey{}, store)
}

// SubAgentSnapshotStoreFromContext returns the per-turn override, or nil.
func SubAgentSnapshotStoreFromContext(ctx context.Context) SubAgentSnapshotStore {
	if ctx == nil {
		return nil
	}
	store, _ := ctx.Value(snapshotStoreKey{}).(SubAgentSnapshotStore)
	return store
}
