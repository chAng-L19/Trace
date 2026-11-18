package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) ActivityWorkspace(id string) (ActivityProjection, error) {
	return s.ActivityWorkspaceContext(context.Background(), id)
}

func (s *Store) ActivityWorkspaceContext(ctx context.Context, id string) (ActivityProjection, error) {
	return s.displayWorkspace(ctx, id, MessagePageQuery{}, "activity")
}

// MessageWorkspace preserves committed message identities and ownership without
// loading evidence sources. Full record/acceptance routes retain their own gates.
func (s *Store) MessageWorkspace(ctx context.Context, id string, query MessagePageQuery) (ActivityProjection, error) {
	if query.Limit == 40 {
		query.Limit = 0
	}
	if query == (MessagePageQuery{}) {
		return s.ActivityWorkspaceContext(ctx, id)
	}
	return s.displayWorkspace(ctx, id, query, "messages")
}

func (s *Store) PlannerWorkspace(ctx context.Context, id string) (ActivityProjection, error) {
	return s.displayWorkspace(ctx, id, MessagePageQuery{}, "planner")
}

const (
	activityCacheMaxEntries = 64
	activityCacheMaxBytes   = 8 << 20
	activityCacheMaxEntry   = 1 << 20
)

// This is disposable display data in the existing Store, never a source
// authority. No entry retains a storedView, workspaceBase, or source slice.
type activityReadCache struct {
	entries map[string]*activityCacheEntry
	bytes   int
	clock   uint64
}

type activityCacheKey struct {
	info    os.FileInfo
	state   [32]byte
	version int64
}

type activityCacheEntry struct {
	key       activityCacheKey
	data, run []byte
	used      uint64
}

func (s *Store) cachedJSONActivity(id string) (*activityCacheEntry, *activityCacheEntry, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) || id == "." || id == ".." {
		return nil, nil, os.ErrNotExist
	}
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	info, err := os.Stat(filepath.Join(s.root, id+".json"))
	if err != nil {
		return nil, nil, err
	}
	hit, prior := s.cachedActivityLocked(id, activityCacheKey{info: info})
	return hit, prior, nil
}

func (s *Store) cachedActivity(id string, key activityCacheKey) (*activityCacheEntry, *activityCacheEntry) {
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	return s.cachedActivityLocked(id, key)
}

func (s *Store) cachedActivityLocked(id string, key activityCacheKey) (*activityCacheEntry, *activityCacheEntry) {
	prior := s.activityCache.entries[id]
	if prior == nil {
		return nil, nil
	}
	match := prior.key.state == key.state
	if !s.sqlite {
		match = sameGeneration(prior.key.info, key.info)
	}
	if !match {
		return nil, prior
	}
	s.activityCache.clock++
	prior.used = s.activityCache.clock
	return prior, prior
}

func (s *Store) cacheActivity(id string, key activityCacheKey, previous *activityCacheEntry, data, run []byte) {
	// The response itself stays complete. Oversized windows simply miss cache.
	size := len(data) + len(run)
	if size > activityCacheMaxEntry {
		return
	}
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	if !s.sqlite {
		info, err := os.Stat(filepath.Join(s.root, id+".json"))
		if err != nil || !sameGeneration(key.info, info) {
			return
		}
	}
	c := &s.activityCache
	// Another read may have published while this miss was being built. Keep
	// that result; a late read must not replace its cache entry.
	if c.entries[id] != previous {
		return
	}
	if previous != nil {
		c.bytes -= len(previous.data) + len(previous.run)
		delete(c.entries, id)
	}
	// ponytail: at most 64 candidates; a list is unnecessary for this small cache.
	for len(c.entries) >= activityCacheMaxEntries || c.bytes+size > activityCacheMaxBytes {
		var oldest string
		var age uint64
		for name, entry := range c.entries {
			if oldest == "" || entry.used < age {
				oldest, age = name, entry.used
			}
		}
		entry := c.entries[oldest]
		c.bytes -= len(entry.data) + len(entry.run)
		delete(c.entries, oldest)
	}
	if c.entries == nil {
		c.entries = make(map[string]*activityCacheEntry)
	}
	c.clock++
	ownedData, ownedRun := make([]byte, len(data)), make([]byte, len(run))
	copy(ownedData, data)
	copy(ownedRun, run)
	c.entries[id] = &activityCacheEntry{key: key, data: ownedData, run: ownedRun, used: c.clock}
	c.bytes += size
}
