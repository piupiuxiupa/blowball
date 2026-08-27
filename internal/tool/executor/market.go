package executor

import (
	"context"
	"os"

	"github.com/lush/blowball/internal/skillmarket"
)

// marketMountRoot is the in-sandbox namespace where per-user skill-market
// skills are mounted read-only, one flattened directory per skill
// (/skills/market/{skill-name}). It deliberately sits beside /skills/global
// in the reserved /skills namespace.
const marketMountRoot = "/skills/market/"

// MarketBind is one per-user skill-market ro-bind: the host directory (under
// {data-dir}/skills-market) and the flattened in-sandbox target path.
type MarketBind struct {
	Host   string
	Target string
}

// WithMarket attaches the skill-market client used to resolve the CURRENT
// user's authorized skills into per-skill --ro-bind mounts (skill-market
// capability). nil (the default) mounts nothing — byte-for-byte pre-capability
// bwrap behavior. The allowlist flows through the per-user TTL cache, so
// mount resolution adds no outbound requests once warm.
func (t *Tools) WithMarket(c *skillmarket.Client) *Tools {
	t.market = c
	return t
}

// marketBinds resolves the calling user's skill-market allowlist into
// stat-guarded ro-binds: one per authorized skill whose directory exists on
// disk. The allowlist is the ONLY authorization surface — the whole market
// dir is NEVER mounted (that would expose other users' market skills to any
// bash caller) — and every failure shape (down market, timeout, missing
// token) fail-closes to zero binds. Disk-sync lag skips the single entry
// (bwrap must not fail to start); the skill's scripts surface not-found at
// run time instead, matching the luban read-path contract.
func (t *Tools) marketBinds(ctx context.Context, userID string) []MarketBind {
	if t.market == nil || userID == "" {
		return nil
	}
	dirs := t.market.Dirs(t.market.Allowlist(ctx, userID))
	binds := make([]MarketBind, 0, len(dirs))
	for _, nd := range dirs {
		if _, err := os.Stat(nd.Dir); err != nil {
			continue // stat guard: unsynced entry, skip the bind
		}
		binds = append(binds, MarketBind{Host: nd.Dir, Target: marketMountRoot + nd.Name})
	}
	return binds
}
