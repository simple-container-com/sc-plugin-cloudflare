package cloudflare

import "sync"

// legacyClaims hands each pre-#13 URN name to one record only. Pulumi rejects
// two resources aliasing the same old URN, and an A plus AAAA, or a
// round-robin of A records, share one legacy name.
type legacyClaims struct {
	mu      sync.Mutex
	claimed map[string]struct{}
}

// claim reports whether name was not claimed before.
func (c *legacyClaims) claim(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.claimed[name]; ok {
		return false
	}
	if c.claimed == nil {
		c.claimed = map[string]struct{}{}
	}
	c.claimed[name] = struct{}{}
	return true
}
