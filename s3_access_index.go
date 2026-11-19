package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	IndexTierDisk = "disk"
	IndexTierS3   = "s3"
	IndexTierBoth = "both"
)

// CacheIndexEntry stores persistent unified metadata and aging timestamps for a cached asset
// across L1 disk ("disk"), L2 S3 ("s3"), or both ("both").
type CacheIndexEntry struct {
	Key           string `json:"key"`
	URL           string `json:"url,omitempty"`
	Size          int64  `json:"size"`
	Tier          string `json:"tier,omitempty"` // "disk", "s3", or "both"
	ContentType   string `json:"content_type,omitempty"`
	ETag          string `json:"etag,omitempty"`
	LastModified  string `json:"last_modified,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	LastValidated int64  `json:"last_validated,omitempty"`
	LastAccessed  int64  `json:"last_accessed"`
}

// S3IndexEntry preserves backwards-compatible naming for legacy callers/tests.
type S3IndexEntry = CacheIndexEntry

// CacheIndex tracks local aging and validation metadata across persistent tiers and sweeps idle objects.
type CacheIndex struct {
	mu         sync.Mutex
	saveMu     sync.Mutex
	indexPath  string
	entries    map[string]*CacheIndexEntry
	dirty      bool
	nowFn      func() time.Time
	stopCh     chan struct{}
	stopOnce   sync.Once
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
}

// S3AccessIndex preserves backwards-compatible naming for legacy callers/tests.
type S3AccessIndex = CacheIndex

// GlobalCacheIndex and GlobalS3Index track persistent assets on host-local STATE_DIR storage.
var (
	GlobalCacheIndex *CacheIndex
	GlobalS3Index    *S3AccessIndex
)

func getGlobalCacheIndex() *CacheIndex {
	if GlobalCacheIndex != nil {
		return GlobalCacheIndex
	}
	return GlobalS3Index
}

// NewCacheIndex constructs a unified CacheIndex persisted at indexPath.
func NewCacheIndex(indexPath string) *CacheIndex {
	return &CacheIndex{
		indexPath: indexPath,
		entries:   make(map[string]*CacheIndexEntry),
		nowFn:     time.Now,
		stopCh:    make(chan struct{}),
	}
}

// NewS3AccessIndex constructs an S3AccessIndex persisted at indexPath.
func NewS3AccessIndex(indexPath string) *S3AccessIndex {
	return NewCacheIndex(indexPath)
}

func mergeIndexTier(existing, added string) string {
	switch {
	case added == "":
		return existing
	case existing == "" || existing == added:
		return added
	case existing == IndexTierBoth || added == IndexTierBoth:
		return IndexTierBoth
	default:
		return IndexTierBoth
	}
}

func (idx *S3AccessIndex) now() time.Time {
	if idx.nowFn != nil {
		return idx.nowFn()
	}
	return time.Now()
}

// Load reads an existing s3_index.json file from disk if present.
func (idx *S3AccessIndex) Load() error {
	if idx == nil || idx.indexPath == "" {
		return nil
	}
	data, err := os.ReadFile(idx.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var loaded map[string]*S3IndexEntry
	if err := json.Unmarshal(data, &loaded); err != nil {
		return err
	}

	idx.mu.Lock()
	for k, e := range loaded {
		if e != nil && k != "" {
			if e.Key == "" {
				e.Key = k
			}
			idx.entries[k] = e
		}
	}
	idx.mu.Unlock()
	return nil
}

// Flush atomically writes the index to disk when dirty.
func (idx *S3AccessIndex) Flush() error {
	if idx == nil || idx.indexPath == "" {
		return nil
	}

	idx.mu.Lock()
	if !idx.dirty {
		idx.mu.Unlock()
		return nil
	}
	data, err := json.MarshalIndent(idx.entries, "", "  ")
	if err != nil {
		idx.mu.Unlock()
		return err
	}
	idx.dirty = false
	idx.mu.Unlock()

	idx.saveMu.Lock()
	defer idx.saveMu.Unlock()

	dir := filepath.Dir(idx.indexPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		idx.markDirty()
		return err
	}

	tmpPath := fmt.Sprintf("%s.tmp.%d", idx.indexPath, time.Now().UnixNano())
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		idx.markDirty()
		return err
	}
	if err := os.Rename(tmpPath, idx.indexPath); err != nil {
		_ = os.Remove(tmpPath)
		idx.markDirty()
		return err
	}
	return nil
}

func (idx *S3AccessIndex) markDirty() {
	idx.mu.Lock()
	idx.dirty = true
	idx.mu.Unlock()
}

// RecordCommit records or updates an S3 object commit and flushes immediately.
func (idx *CacheIndex) RecordCommit(key, targetURL string, size int64) {
	idx.RecordCommitForTier(key, FileMeta{URL: targetURL, TotalSize: size}, IndexTierS3)
}

// RecordCommitForTier records a newly committed object on a specific tier and flushes immediately.
func (idx *CacheIndex) RecordCommitForTier(key string, meta FileMeta, tier string) {
	if idx == nil || key == "" {
		return
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if !ok {
		e = &CacheIndexEntry{
			Key:       key,
			CreatedAt: nowUnix,
		}
		idx.entries[key] = e
	}
	if meta.URL != "" {
		e.URL = meta.URL
	}
	if meta.TotalSize > 0 {
		e.Size = meta.TotalSize
	}
	if meta.ContentType != "" {
		e.ContentType = meta.ContentType
	}
	if meta.ETag != "" {
		e.ETag = meta.ETag
	}
	if meta.LastModified != "" {
		e.LastModified = meta.LastModified
	}
	if meta.LastValidated > 0 {
		e.LastValidated = meta.LastValidated
	} else if e.LastValidated == 0 {
		e.LastValidated = nowUnix
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = nowUnix
	}
	e.LastAccessed = nowUnix
	e.Tier = mergeIndexTier(e.Tier, tier)
	idx.dirty = true
	idx.mu.Unlock()

	_ = idx.Flush()
}

// RecordAccess refreshes LastAccessed on real playback (preserving existing tier or defaulting to S3).
func (idx *CacheIndex) RecordAccess(key, targetURL string, size int64) {
	idx.RecordAccessForTier(key, targetURL, size, "")
}

// RecordAccessForTier refreshes LastAccessed on real playback for key and merges tier membership.
func (idx *CacheIndex) RecordAccessForTier(key, targetURL string, size int64, tier string) {
	if idx == nil || key == "" {
		return
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if !ok {
		t := tier
		if t == "" {
			t = IndexTierS3
		}
		e = &CacheIndexEntry{
			Key:       key,
			Tier:      t,
			CreatedAt: nowUnix,
		}
		idx.entries[key] = e
	} else if tier != "" {
		merged := mergeIndexTier(e.Tier, tier)
		if merged != e.Tier {
			e.Tier = merged
			idx.dirty = true
		}
	} else if e.Tier == "" {
		e.Tier = IndexTierS3
		idx.dirty = true
	}
	if targetURL != "" && e.URL == "" {
		e.URL = targetURL
	}
	if size > 0 && e.Size == 0 {
		e.Size = size
	}
	if e.LastAccessed < nowUnix {
		e.LastAccessed = nowUnix
		idx.dirty = true
	}
	idx.mu.Unlock()
}

// RecordValidation updates metadata & LastValidated without modifying LastAccessed on existing entries.
func (idx *CacheIndex) RecordValidation(key string, meta FileMeta, tier string) {
	if idx == nil || key == "" {
		return
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if !ok {
		lastAcc := nowUnix
		created := nowUnix
		if meta.LastValidated > 0 {
			lastAcc = meta.LastValidated
			created = meta.LastValidated
		}
		e = &CacheIndexEntry{
			Key:          key,
			Tier:         tier,
			CreatedAt:    created,
			LastAccessed: lastAcc,
		}
		idx.entries[key] = e
	} else if tier != "" {
		e.Tier = mergeIndexTier(e.Tier, tier)
	}
	if meta.URL != "" {
		e.URL = meta.URL
	}
	if meta.TotalSize > 0 {
		e.Size = meta.TotalSize
	}
	if meta.ContentType != "" {
		e.ContentType = meta.ContentType
	}
	if meta.ETag != "" {
		e.ETag = meta.ETag
	}
	if meta.LastModified != "" {
		e.LastModified = meta.LastModified
	}
	if meta.LastValidated > 0 {
		e.LastValidated = meta.LastValidated
	} else if e.LastValidated == 0 {
		e.LastValidated = nowUnix
	}
	if e.LastAccessed == 0 {
		e.LastAccessed = nowUnix
	}
	idx.dirty = true
	idx.mu.Unlock()

	_ = idx.Flush()
}

// MarkAddedToTier merges tier membership onto key and flushes.
func (idx *CacheIndex) MarkAddedToTier(key, tier string) {
	if idx == nil || key == "" || tier == "" {
		return
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if !ok {
		e = &CacheIndexEntry{
			Key:          key,
			Tier:         tier,
			CreatedAt:    nowUnix,
			LastAccessed: nowUnix,
		}
		idx.entries[key] = e
	} else {
		e.Tier = mergeIndexTier(e.Tier, tier)
	}
	idx.dirty = true
	idx.mu.Unlock()

	_ = idx.Flush()
}

// MarkRemovedFromTier updates or removes key's entry when a copy is deleted from tier.
func (idx *CacheIndex) MarkRemovedFromTier(key, tier string) {
	if idx == nil || key == "" || tier == "" {
		return
	}

	idx.mu.Lock()
	e, ok := idx.entries[key]
	changed := false
	if ok {
		switch tier {
		case IndexTierDisk:
			if e.Tier == IndexTierBoth {
				e.Tier = IndexTierS3
				changed = true
			} else if e.Tier == IndexTierDisk || e.Tier == "" {
				delete(idx.entries, key)
				changed = true
			}
		case IndexTierS3:
			if e.Tier == IndexTierBoth {
				e.Tier = IndexTierDisk
				changed = true
			} else if e.Tier == IndexTierS3 || e.Tier == "" {
				delete(idx.entries, key)
				changed = true
			}
		}
		if changed {
			idx.dirty = true
		}
	}
	idx.mu.Unlock()

	if changed {
		_ = idx.Flush()
	}
}

// Get returns a copy of the CacheIndexEntry for key if present.
func (idx *CacheIndex) Get(key string) (*CacheIndexEntry, bool) {
	if idx == nil || key == "" {
		return nil, false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	e, ok := idx.entries[key]
	if !ok || e == nil {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// GetLastAccessed returns the recorded LastAccessed timestamp for key.
func (idx *CacheIndex) GetLastAccessed(key string) (time.Time, bool) {
	if idx == nil || key == "" {
		return time.Time{}, false
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	e, ok := idx.entries[key]
	if !ok || e == nil || e.LastAccessed <= 0 {
		return time.Time{}, false
	}
	return time.Unix(e.LastAccessed, 0), true
}

// GetTier returns the recorded tier membership for key ("disk", "s3", "both", or "").
func (idx *CacheIndex) GetTier(key string) string {
	if idx == nil || key == "" {
		return ""
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	e, ok := idx.entries[key]
	if !ok || e == nil {
		return ""
	}
	return e.Tier
}

// TouchIfPresent refreshes LastAccessed if key is already tracked in the index.
func (idx *S3AccessIndex) TouchIfPresent(key string) bool {
	if idx == nil || key == "" {
		return false
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if ok && e.LastAccessed < nowUnix {
		e.LastAccessed = nowUnix
		idx.dirty = true
	}
	idx.mu.Unlock()
	return ok
}

// TouchChain refreshes LastAccessed for every upcoming episode URL already tracked in S3.
func (idx *S3AccessIndex) TouchChain(urls []string) int {
	if idx == nil || len(urls) == 0 {
		return 0
	}
	touched := 0
	for _, u := range urls {
		if u == "" {
			continue
		}
		k, ext := getCacheKeyAndExt(u)
		if idx.TouchIfPresent(k + ext) {
			touched++
		}
	}
	return touched
}

// Delete removes a key from the index when evicted outside the sweeper (e.g. revalidation).
func (idx *S3AccessIndex) Delete(key string) {
	if idx == nil || key == "" {
		return
	}
	idx.mu.Lock()
	_, ok := idx.entries[key]
	if ok {
		delete(idx.entries, key)
		idx.dirty = true
	}
	idx.mu.Unlock()

	if ok {
		_ = idx.Flush()
	}
}

// Stats returns the tracked object count and total bytes.
func (idx *S3AccessIndex) Stats() (int, int64) {
	if idx == nil {
		return 0, 0
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var totalBytes int64
	for _, e := range idx.entries {
		totalBytes += e.Size
	}
	return len(idx.entries), totalBytes
}

// StatsByTier returns object counts and bytes broken down across disk, s3, and both tiers.
func (idx *CacheIndex) StatsByTier() (diskCount int, diskBytes int64, s3Count int, s3Bytes int64, bothCount int) {
	if idx == nil {
		return 0, 0, 0, 0, 0
	}
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for _, e := range idx.entries {
		switch e.Tier {
		case IndexTierDisk:
			diskCount++
			diskBytes += e.Size
		case IndexTierBoth:
			diskCount++
			diskBytes += e.Size
			s3Count++
			s3Bytes += e.Size
			bothCount++
		default:
			// "" (legacy s3_index entries) or IndexTierS3
			s3Count++
			s3Bytes += e.Size
		}
	}
	return diskCount, diskBytes, s3Count, s3Bytes, bothCount
}

// Reconcile synchronizes the unified index against local disk, S3, or tiered storage.
func (idx *CacheIndex) Reconcile(ctx context.Context, storage StorageBackend) error {
	if idx == nil || storage == nil {
		return nil
	}

	nowUnix := idx.now().Unix()
	type observedEntry struct {
		size      int64
		modTime   time.Time
		tier      string
		localMeta *FileMeta
	}
	live := make(map[string]*observedEntry)

	switch b := storage.(type) {
	case *TieredStorageBackend:
		if b.Local() != nil && !IsNASHealthy() {
			return ErrStorageCircuitOpen
		}
		if b.S3() != nil && !IsS3Healthy() {
			return ErrStorageCircuitOpen
		}
		var localObjs, s3Objs []StorageObjectInfo
		var err error
		if b.Local() != nil {
			localObjs, err = b.Local().List(ctx)
			if err != nil {
				RecordNASFailure(err)
				return err
			}
			RecordNASSuccess()
		}
		if b.S3() != nil {
			s3Objs, err = b.S3().List(ctx)
			if err != nil {
				RecordS3Failure(err)
				return err
			}
			RecordS3Success()
		}
		for _, obj := range localObjs {
			var lm *FileMeta
			if m, _, ok, serr := b.Local().StatWithTier(ctx, obj.Key); serr == nil && ok {
				lm = m
			}
			live[obj.Key] = &observedEntry{
				size:      obj.Size,
				modTime:   obj.LastModified,
				tier:      IndexTierDisk,
				localMeta: lm,
			}
		}
		for _, obj := range s3Objs {
			if existing, ok := live[obj.Key]; ok {
				existing.tier = IndexTierBoth
				if existing.size <= 0 {
					existing.size = obj.Size
				}
			} else {
				live[obj.Key] = &observedEntry{
					size:    obj.Size,
					modTime: obj.LastModified,
					tier:    IndexTierS3,
				}
			}
		}

	case *LocalStorageBackend:
		if !IsNASHealthy() {
			return ErrStorageCircuitOpen
		}
		objs, err := b.List(ctx)
		if err != nil {
			RecordNASFailure(err)
			return err
		}
		RecordNASSuccess()
		for _, obj := range objs {
			var lm *FileMeta
			if m, _, ok, serr := b.StatWithTier(ctx, obj.Key); serr == nil && ok {
				lm = m
			}
			live[obj.Key] = &observedEntry{
				size:      obj.Size,
				modTime:   obj.LastModified,
				tier:      IndexTierDisk,
				localMeta: lm,
			}
		}

	default:
		if !IsS3Healthy() {
			return ErrStorageCircuitOpen
		}
		objs, err := storage.List(ctx)
		if err != nil {
			RecordS3Failure(err)
			return err
		}
		RecordS3Success()
		for _, obj := range objs {
			live[obj.Key] = &observedEntry{
				size:    obj.Size,
				modTime: obj.LastModified,
				tier:    IndexTierS3,
			}
		}
	}

	idx.mu.Lock()
	added, removed, updated := 0, 0, 0
	for key, obs := range live {
		modUnix := nowUnix
		if !obs.modTime.IsZero() {
			modUnix = obs.modTime.Unix()
		}
		e, ok := idx.entries[key]
		if !ok {
			entry := &CacheIndexEntry{
				Key:           key,
				Size:          obs.size,
				Tier:          obs.tier,
				CreatedAt:     modUnix,
				LastValidated: modUnix,
				LastAccessed:  modUnix,
			}
			if obs.localMeta != nil {
				if obs.localMeta.URL != "" {
					entry.URL = obs.localMeta.URL
				}
				if obs.localMeta.ContentType != "" {
					entry.ContentType = obs.localMeta.ContentType
				}
				if obs.localMeta.ETag != "" {
					entry.ETag = obs.localMeta.ETag
				}
				if obs.localMeta.LastModified != "" {
					entry.LastModified = obs.localMeta.LastModified
				}
				if obs.localMeta.LastValidated > 0 {
					entry.LastValidated = obs.localMeta.LastValidated
					entry.LastAccessed = obs.localMeta.LastValidated
				}
			}
			idx.entries[key] = entry
			added++
		} else {
			if obs.size > 0 && e.Size != obs.size {
				e.Size = obs.size
				updated++
			}
			if obs.tier != "" && e.Tier != obs.tier {
				e.Tier = obs.tier
				updated++
			}
			if obs.localMeta != nil {
				if e.URL == "" && obs.localMeta.URL != "" {
					e.URL = obs.localMeta.URL
					updated++
				}
				if e.ContentType == "" && obs.localMeta.ContentType != "" {
					e.ContentType = obs.localMeta.ContentType
					updated++
				}
				if e.ETag == "" && obs.localMeta.ETag != "" {
					e.ETag = obs.localMeta.ETag
					updated++
				}
				if e.LastModified == "" && obs.localMeta.LastModified != "" {
					e.LastModified = obs.localMeta.LastModified
					updated++
				}
			}
		}
	}

	for key := range idx.entries {
		if _, ok := live[key]; !ok {
			delete(idx.entries, key)
			removed++
		}
	}

	if added > 0 || removed > 0 || updated > 0 {
		idx.dirty = true
	}
	totalCount := len(idx.entries)
	idx.mu.Unlock()

	if added > 0 || removed > 0 || updated > 0 {
		_ = idx.Flush()
		log.Printf("[CacheIndex] Reconciled (%s): tracked=%d (added=%d, pruned=%d, updated=%d)",
			storage.Type(), totalCount, added, removed, updated)
	}
	return nil
}

// SweepExpired evicts objects whose LastAccessed exceeds maxIdleAge across whichever tier(s) hold them,
// skipping actively used keys.
func (idx *CacheIndex) SweepExpired(ctx context.Context, storage StorageBackend, registry *CacheRegistry, maxIdleAge time.Duration) (int, int64) {
	if idx == nil || storage == nil || maxIdleAge <= 0 {
		return 0, 0
	}

	if err := idx.Reconcile(ctx, storage); err != nil {
		log.Printf("[IdleEvictor] Skipping sweep: reconcile failed: %v", err)
		return 0, 0
	}

	now := idx.now()

	idx.mu.Lock()
	var candidates []CacheIndexEntry
	for _, e := range idx.entries {
		idle := now.Sub(time.Unix(e.LastAccessed, 0))
		if idle >= maxIdleAge {
			candidates = append(candidates, *e)
		}
	}
	idx.mu.Unlock()

	if len(candidates) == 0 {
		return 0, 0
	}

	// Evict oldest-accessed objects first for deterministic logs
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].LastAccessed == candidates[j].LastAccessed {
			return candidates[i].Key < candidates[j].Key
		}
		return candidates[i].LastAccessed < candidates[j].LastAccessed
	})

	evicted := 0
	var freedBytes int64

	for _, cand := range candidates {
		if ctx.Err() != nil {
			break
		}
		if registry != nil && registry.IsKeyInUse(cand.Key) {
			continue
		}

		// Re-check under idx.mu so a concurrent playback touch isn't overwritten by delete
		idx.mu.Lock()
		cur, stillTracked := idx.entries[cand.Key]
		stillExpired := stillTracked && idx.now().Sub(time.Unix(cur.LastAccessed, 0)) >= maxIdleAge
		curTier := ""
		if cur != nil {
			curTier = cur.Tier
		}
		idx.mu.Unlock()
		if !stillExpired {
			continue
		}

		if err := storage.Delete(ctx, cand.Key); err != nil {
			log.Printf("[IdleEvictor] Failed to delete %s (%s): %v", cand.Key, curTier, err)
			continue
		}

		if registry != nil && cand.Size > 0 && (curTier == IndexTierDisk || curTier == IndexTierBoth || storage.Type() == "disk") {
			registry.UpdateSize(-cand.Size)
		}

		idx.mu.Lock()
		delete(idx.entries, cand.Key)
		idx.dirty = true
		idx.mu.Unlock()

		idle := idx.now().Sub(time.Unix(cand.LastAccessed, 0)).Round(time.Second)
		evicted++
		freedBytes += cand.Size
		log.Printf("[IdleEvictor] Evicted idle object: %s (Tier: %s, Size: %.2f MB, Idle: %s)",
			cand.Key, curTier, float64(cand.Size)/(1024*1024), idle)
	}

	if evicted > 0 {
		_ = idx.Flush()
		log.Printf("[IdleEvictor] Sweep complete. Evicted %d objects, freed %.2f MB",
			evicted, float64(freedBytes)/(1024*1024))
	}

	return evicted, freedBytes
}

// StartBackgroundLoops starts periodic flushing and idle-age S3 sweeping.
func (idx *S3AccessIndex) StartBackgroundLoops(storage StorageBackend, registry *CacheRegistry) {
	if idx == nil || storage == nil {
		return
	}

	// Capture config reference and parameters on the caller goroutine so background
	// loops never race on GlobalConfig pointer reassignment in tests.
	cfg := GlobalConfig
	maxIdle := 720 * time.Hour
	interval := 6 * time.Hour
	if cfg != nil {
		maxIdle = cfg.GetCacheMaxIdleAge()
		interval = cfg.GetCacheEvictInterval()
	}

	loopCtx, loopCancel := context.WithCancel(context.Background())
	idx.mu.Lock()
	idx.loopCancel = loopCancel
	idx.mu.Unlock()

	sweepStale := func(ctx context.Context) {
		if cleaner, ok := storage.(ChunkCleanupBackend); ok {
			if deleted, err := cleaner.SweepStaleChunks(ctx, 24*time.Hour); err != nil {
				if loopCtx.Err() == nil {
					log.Printf("[S3Index] Stale chunk sweep failed: %v", err)
				}
			} else if deleted > 0 {
				log.Printf("[S3Index] Swept %d stale staging chunks (>24h old)", deleted)
			}
		}
	}

	// Initial startup reconcile + sweep
	idx.wg.Add(1)
	go func() {
		defer idx.wg.Done()
		ctx, cancel := context.WithTimeout(loopCtx, 2*time.Minute)
		defer cancel()

		if maxIdle > 0 {
			idx.SweepExpired(ctx, storage, registry, maxIdle)
		} else if err := idx.Reconcile(ctx, storage); err != nil && loopCtx.Err() == nil {
			log.Printf("[S3Index] Initial reconcile failed: %v", err)
		}
		sweepStale(ctx)
	}()

	// Periodic metadata flush for playback touches
	idx.wg.Add(1)
	go func() {
		defer idx.wg.Done()
		flushTicker := time.NewTicker(60 * time.Second)
		defer flushTicker.Stop()
		for {
			select {
			case <-idx.stopCh:
				_ = idx.Flush()
				return
			case <-flushTicker.C:
				_ = idx.Flush()
			}
		}
	}()

	// Periodic idle-age sweep
	idx.wg.Add(1)
	go func() {
		defer idx.wg.Done()
		evictTicker := time.NewTicker(interval)
		defer evictTicker.Stop()
		for {
			select {
			case <-idx.stopCh:
				return
			case <-evictTicker.C:
				curMaxIdle := 720 * time.Hour
				if cfg != nil {
					curMaxIdle = cfg.GetCacheMaxIdleAge()
				}
				ctx, cancel := context.WithTimeout(loopCtx, 5*time.Minute)
				if curMaxIdle > 0 {
					idx.SweepExpired(ctx, storage, registry, curMaxIdle)
				}
				sweepStale(ctx)
				cancel()
			}
		}
	}()
}

// Stop terminates background loops and flushes pending index updates.
func (idx *S3AccessIndex) Stop() {
	if idx == nil {
		return
	}
	idx.stopOnce.Do(func() {
		idx.mu.Lock()
		cancel := idx.loopCancel
		idx.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		close(idx.stopCh)
	})
	idx.wg.Wait()
}
