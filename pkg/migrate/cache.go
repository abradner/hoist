package migrate

import (
	"context"
	"sync"
)

// Cache memoises deltas and revisions for one process: the tag picker, the deploy confirm and
// the plan confirm all ask about the same pairs, and a sha pair's delta is immutable. Errors
// are never cached — a rate-limit 403 must not stick for the rest of the session. Keys carry
// the app repo even though every Comparer is per-repo, because one Cache serves every mapped
// image repo.
type Cache struct {
	mu     sync.Mutex
	deltas map[DeltaKey]Delta
	revs   map[RevisionKey]Revision
}

// DeltaKey identifies one delta: app repo, both shas, and the prefix it was attributed under.
type DeltaKey struct {
	AppRepo, FromSHA, ToSHA, Prefix string
}

// RevisionKey identifies one resolution: app repo and the image reference as written.
type RevisionKey struct {
	AppRepo, Ref string
}

// Delta returns the cached delta for key or computes it with fill and caches the result.
func (c *Cache) Delta(ctx context.Context, key DeltaKey, fill func(context.Context) (Delta, error)) (Delta, error) {
	c.mu.Lock()
	if d, ok := c.deltas[key]; ok {
		c.mu.Unlock()
		return d, nil
	}
	c.mu.Unlock()
	d, err := fill(ctx)
	if err != nil {
		return d, err
	}
	c.mu.Lock()
	if c.deltas == nil {
		c.deltas = map[DeltaKey]Delta{}
	}
	c.deltas[key] = d
	c.mu.Unlock()
	return d, nil
}

// Revision is Delta's twin for resolutions. An unresolved Revision (SourceUnknown, nil error)
// IS cached: it is an answer, not a failure, and re-asking the forge would give the same one.
func (c *Cache) Revision(ctx context.Context, key RevisionKey, fill func(context.Context) (Revision, error)) (Revision, error) {
	c.mu.Lock()
	if r, ok := c.revs[key]; ok {
		c.mu.Unlock()
		return r, nil
	}
	c.mu.Unlock()
	r, err := fill(ctx)
	if err != nil {
		return r, err
	}
	c.mu.Lock()
	if c.revs == nil {
		c.revs = map[RevisionKey]Revision{}
	}
	c.revs[key] = r
	c.mu.Unlock()
	return r, nil
}
