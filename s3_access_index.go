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
	Key                  string `json:"key"`
	URL                  string `json:"url,omitempty"`
	Size                 int64  `json:"size"`
	Tier                 string `json:"tier,omitempty"` // "disk", "s3", or "both"
	ContentType          string `json:"content_type,omitempty"`
	ETag                 string `json:"etag,omitempty"`
	LastModified         string `json:"last_modified,omitempty"`
	CreatedAt            int64  `json:"created_at"`
	LastValidated        int64  `json:"last_validated,omitempty"`
	LastAccessed         int64  `json:"last_accessed"`
	PendingRecompose     bool   `json:"pending_recompose,omitempty"`
	ChunkSize            int64  `json:"chunk_size,omitempty"`
	TotalParts           int    `json:"total_parts,omitempty"`
	RecomposeAttempts    int    `json:"recompose_attempts,omitempty"`
	LastRecomposeAttempt int64  `json:"last_recompose_attempt,omitempty"`
}

// S3IndexEntry preserves backwards-compatible naming for legacy callers/tests.
type S3IndexEntry = CacheIndexEntry

const (
	pendingRecomposeRetention = 7 * 24 * time.Hour
)

// CacheIndex tracks local aging and validation metadata across persistent tiers and sweeps idle objects.
type CacheIndex struct {
	mu                sync.Mutex
	saveMu            sync.Mutex
	indexPath         string
	entries           map[string]*CacheIndexEntry
	dirty             bool
	nowFn             func() time.Time
	stopCh            chan struct{}
	stopOnce          sync.Once
	loopCancel        context.CancelFunc
	recomposeTrigger  chan struct{}
	sizeEvictTrigger  chan struct{}
	recomposeInFlight map[string]bool
	wg                sync.WaitGroup
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
		indexPath:         indexPath,
		entries:           make(map[string]*CacheIndexEntry),
		nowFn:             time.Now,
		stopCh:            make(chan struct{}),
		recomposeTrigger:  make(chan struct{}, 1),
		sizeEvictTrigger:  make(chan struct{}, 1),
		recomposeInFlight: make(map[string]bool),
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
	if tier == IndexTierS3 || tier == IndexTierBoth {
		e.PendingRecompose = false
		e.ChunkSize = 0
		e.TotalParts = 0
		e.RecomposeAttempts = 0
		e.LastRecomposeAttempt = 0
	}
	idx.dirty = true
	idx.mu.Unlock()

	_ = idx.Flush()
	if tier == IndexTierS3 || tier == IndexTierBoth {
		idx.TriggerS3SizeCheck()
	}
}

// TriggerS3SizeCheck signals the background eviction loop to enforce S3_MAX_BYTES without extra S3 List calls.
func (idx *CacheIndex) TriggerS3SizeCheck() {
	if idx == nil || idx.sizeEvictTrigger == nil {
		return
	}
	select {
	case idx.sizeEvictTrigger <- struct{}{}:
	default:
	}
}

// RecordPendingRecompose records an asset whose chunks are 100% uploaded to S3 but not yet
// sealed into vod/<key>, persisting it so chunked Cache HITs and background retries survive failures/restarts.
func (idx *CacheIndex) RecordPendingRecompose(key string, meta FileMeta, chunkSize int64, totalParts int, recordAttempt bool) {
	if idx == nil || key == "" || chunkSize <= 0 || totalParts <= 0 {
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
	if e.LastAccessed == 0 {
		e.LastAccessed = nowUnix
	}
	// Do not promote Tier to "s3" until vod/<key> is actually sealed.
	e.PendingRecompose = true
	e.ChunkSize = chunkSize
	e.TotalParts = totalParts
	if recordAttempt {
		e.RecomposeAttempts++
		e.LastRecomposeAttempt = nowUnix
	}
	idx.dirty = true
	idx.mu.Unlock()

	_ = idx.Flush()
}

// ClearPendingRecompose removes a pending-recompose state (deleting the entry if it is not backed on disk/S3).
func (idx *CacheIndex) ClearPendingRecompose(key string) {
	if idx == nil || key == "" {
		return
	}
	idx.mu.Lock()
	e, ok := idx.entries[key]
	changed := false
	if ok && e.PendingRecompose {
		e.PendingRecompose = false
		e.ChunkSize = 0
		e.TotalParts = 0
		e.RecomposeAttempts = 0
		e.LastRecomposeAttempt = 0
		if e.Tier == "" {
			delete(idx.entries, key)
		}
		idx.dirty = true
		changed = true
	}
	idx.mu.Unlock()
	if changed {
		_ = idx.Flush()
	}
}

// IsPendingRecompose reports whether key has uploaded S3 chunks awaiting recomposition within retention.
func (idx *CacheIndex) IsPendingRecompose(key string) bool {
	if idx == nil || key == "" {
		return false
	}
	now := idx.now()
	idx.mu.Lock()
	defer idx.mu.Unlock()
	e, ok := idx.entries[key]
	if !ok || e == nil || !e.PendingRecompose {
		return false
	}
	if e.CreatedAt > 0 && now.Sub(time.Unix(e.CreatedAt, 0)) >= pendingRecomposeRetention {
		return false
	}
	return true
}

// TriggerPendingRecompose wakes the background pending-recompose worker (e.g. after S3 recovery).
func (idx *CacheIndex) TriggerPendingRecompose() {
	if idx == nil || idx.recomposeTrigger == nil {
		return
	}
	select {
	case idx.recomposeTrigger <- struct{}{}:
	default:
	}
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
	} else if !e.PendingRecompose && tier != "" {
		merged := mergeIndexTier(e.Tier, tier)
		if merged != e.Tier {
			e.Tier = merged
			idx.dirty = true
		}
	} else if !e.PendingRecompose && e.Tier == "" {
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
	} else if !e.PendingRecompose && tier != "" {
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
		if e.PendingRecompose && e.Tier == "" {
			continue
		}
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
			if e.PendingRecompose && (obs.tier == IndexTierS3 || obs.tier == IndexTierBoth) {
				e.PendingRecompose = false
				e.ChunkSize = 0
				e.TotalParts = 0
				e.RecomposeAttempts = 0
				e.LastRecomposeAttempt = 0
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

	for key, e := range idx.entries {
		if _, ok := live[key]; !ok {
			if e != nil && e.PendingRecompose {
				continue
			}
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
		if e.PendingRecompose {
			continue
		}
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

// SweepS3OverCapacity evicts least-recently-used S3 objects whenever total S3 usage exceeds maxBytes,
// preserving warm L1 disk files in hybrid mode and skipping in-use or pending-recompose keys.
func (idx *CacheIndex) SweepS3OverCapacity(ctx context.Context, storage StorageBackend, registry *CacheRegistry, maxBytes int64, skipReconcile bool) (int, int64) {
	if idx == nil || storage == nil || maxBytes <= 0 || !IsS3Healthy() {
		return 0, 0
	}
	s3b := extractS3Backend(storage)
	if s3b == nil {
		return 0, 0
	}

	if !skipReconcile {
		if err := idx.Reconcile(ctx, storage); err != nil {
			log.Printf("[S3SizeEvictor] Skipping sweep: reconcile failed: %v", err)
			return 0, 0
		}
	}

	idx.mu.Lock()
	var totalS3Bytes int64
	var candidates []CacheIndexEntry
	storageType := storage.Type()
	for _, e := range idx.entries {
		if e == nil || e.PendingRecompose {
			continue
		}
		isS3 := e.Tier == IndexTierS3 || e.Tier == IndexTierBoth || (e.Tier == "" && storageType == "s3")
		if !isS3 {
			continue
		}
		totalS3Bytes += e.Size
		candidates = append(candidates, *e)
	}
	idx.mu.Unlock()

	if totalS3Bytes <= maxBytes || len(candidates) == 0 {
		return 0, 0
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].LastAccessed != candidates[j].LastAccessed {
			return candidates[i].LastAccessed < candidates[j].LastAccessed
		}
		if candidates[i].CreatedAt != candidates[j].CreatedAt {
			return candidates[i].CreatedAt < candidates[j].CreatedAt
		}
		return candidates[i].Key < candidates[j].Key
	})

	evicted := 0
	var freedBytes int64

	for _, cand := range candidates {
		if totalS3Bytes <= maxBytes || ctx.Err() != nil || !IsS3Healthy() {
			break
		}
		if registry != nil && registry.IsKeyInUse(cand.Key) {
			continue
		}

		idx.mu.Lock()
		cur, stillTracked := idx.entries[cand.Key]
		stillEligible := stillTracked && cur != nil && !cur.PendingRecompose &&
			(cur.Tier == IndexTierS3 || cur.Tier == IndexTierBoth || (cur.Tier == "" && storageType == "s3")) &&
			cur.LastAccessed == cand.LastAccessed
		curTier := ""
		if cur != nil {
			curTier = cur.Tier
		}
		idx.mu.Unlock()
		if !stillEligible {
			continue
		}

		if err := s3b.Delete(ctx, cand.Key); err != nil {
			RecordS3Failure(err)
			log.Printf("[S3SizeEvictor] Failed to delete %s (%s) from S3: %v", cand.Key, curTier, err)
			if !IsS3Healthy() {
				break
			}
			continue
		}
		RecordS3Success()

		idx.mu.Lock()
		if cur, stillExists := idx.entries[cand.Key]; stillExists && cur != nil {
			if cur.Tier == IndexTierBoth {
				cur.Tier = IndexTierDisk
				cur.PendingRecompose = false
			} else {
				delete(idx.entries, cand.Key)
			}
			idx.dirty = true
		}
		idx.mu.Unlock()

		totalS3Bytes -= cand.Size
		evicted++
		freedBytes += cand.Size
		log.Printf("[S3SizeEvictor] Evicted LRU S3 object: %s (TierWas: %s, Size: %.2f MB, RemainingS3: %.2f GB / Limit: %.2f GB)",
			cand.Key, curTier, float64(cand.Size)/(1024*1024),
			float64(totalS3Bytes)/(1024*1024*1024), float64(maxBytes)/(1024*1024*1024))
	}

	if evicted > 0 {
		_ = idx.Flush()
		log.Printf("[S3SizeEvictor] Sweep complete. Evicted %d S3 objects, freed %.2f MB (RemainingS3: %.2f GB / Limit: %.2f GB)",
			evicted, float64(freedBytes)/(1024*1024),
			float64(totalS3Bytes)/(1024*1024*1024), float64(maxBytes)/(1024*1024*1024))
	}

	return evicted, freedBytes
}

// ProcessPendingRecomposes retries sealing any PendingRecompose entries whose chunks remain in S3.
// If bypassBackoff is true (e.g. on S3 recovery or startup), eligible entries are retried immediately.
func (idx *CacheIndex) ProcessPendingRecomposes(ctx context.Context, storage StorageBackend, registry *CacheRegistry, bypassBackoff bool) int {
	if idx == nil || storage == nil || !IsS3Healthy() {
		return 0
	}
	s3b := extractS3Backend(storage)
	if s3b == nil {
		return 0
	}

	now := idx.now()
	idx.mu.Lock()
	var due []CacheIndexEntry
	for _, e := range idx.entries {
		if e == nil || !e.PendingRecompose || e.TotalParts <= 0 || e.ChunkSize <= 0 {
			continue
		}
		if idx.recomposeInFlight != nil && idx.recomposeInFlight[e.Key] {
			continue
		}
		if !bypassBackoff && e.LastRecomposeAttempt > 0 {
			shift := e.RecomposeAttempts - 1
			if shift < 0 {
				shift = 0
			}
			if shift > 4 {
				shift = 4
			}
			backoff := time.Minute << shift // 1m, 2m, 4m, 8m, 16m
			if backoff > 15*time.Minute {
				backoff = 15 * time.Minute
			}
			if now.Sub(time.Unix(e.LastRecomposeAttempt, 0)) < backoff {
				continue
			}
		}
		due = append(due, *e)
	}
	idx.mu.Unlock()

	if len(due) == 0 {
		return 0
	}

	sealed := 0
	for _, entry := range due {
		if ctx.Err() != nil || !IsS3Healthy() {
			break
		}
		if registry != nil {
			registry.Lock.RLock()
			fs, exists := registry.Files[entry.Key]
			registry.Lock.RUnlock()
			if exists && fs != nil {
				fs.Lock.Lock()
				committing := fs.IsCommitting
				writerActive := fs.WriterActive
				fs.Lock.Unlock()
				if committing || writerActive {
					continue
				}
			}
		}

		idx.mu.Lock()
		if idx.recomposeInFlight == nil {
			idx.recomposeInFlight = make(map[string]bool)
		}
		if idx.recomposeInFlight[entry.Key] {
			idx.mu.Unlock()
			continue
		}
		idx.recomposeInFlight[entry.Key] = true
		idx.mu.Unlock()

		meta := FileMeta{
			URL:           entry.URL,
			TotalSize:     entry.Size,
			ContentType:   entry.ContentType,
			ETag:          entry.ETag,
			LastModified:  entry.LastModified,
			LastValidated: entry.LastValidated,
			Completed:     true,
		}
		if meta.LastValidated == 0 {
			meta.LastValidated = idx.now().Unix()
		}

		// 1. Check if vod/<key> was already sealed by a previous attempt that timed out late.
		if existsMono, err := s3b.HasMonolithic(ctx, entry.Key); err == nil && existsMono {
			idx.RecordCommitForTier(entry.Key, meta, IndexTierS3)
			delCtx, delCancel := context.WithTimeout(ctx, 30*time.Second)
			_ = s3b.DeleteChunks(delCtx, entry.Key)
			delCancel()
			sealed++
			log.Printf("[PendingRecompose] Monolithic vod/%s already existed; cleared pending state and cleaned up staging chunks", entry.Key)
			idx.mu.Lock()
			delete(idx.recomposeInFlight, entry.Key)
			idx.mu.Unlock()
			continue
		}

		// 2. Retry server-side Recompose from existing S3 chunks.
		log.Printf("[PendingRecompose] Retrying S3 recomposition for %s (%d parts, attempt #%d)...", entry.Key, entry.TotalParts, entry.RecomposeAttempts+1)
		recompErr := s3b.Recompose(ctx, entry.Key, entry.TotalParts, meta)

		idx.mu.Lock()
		delete(idx.recomposeInFlight, entry.Key)
		idx.mu.Unlock()

		if recompErr == nil {
			idx.RecordCommitForTier(entry.Key, meta, IndexTierS3)
			sealed++
			log.Printf("[PendingRecompose] Successfully sealed vod/%s on retry", entry.Key)
			if GlobalLogBroker != nil {
				go GlobalLogBroker.BroadcastStateUpdate()
			}
			continue
		}

		if isS3NotFoundError(recompErr) {
			log.Printf("[PendingRecompose] Permanent missing chunk for %s (%v); clearing pending entry and cleaning up remaining chunks", entry.Key, recompErr)
			idx.ClearPendingRecompose(entry.Key)
			delCtx, delCancel := context.WithTimeout(ctx, 30*time.Second)
			_ = s3b.DeleteChunks(delCtx, entry.Key)
			delCancel()
			continue
		}

		log.Printf("[PendingRecompose] Retry failed for %s: %v (will retry with backoff)", entry.Key, recompErr)
		idx.RecordPendingRecompose(entry.Key, meta, entry.ChunkSize, entry.TotalParts, true)
	}
	return sealed
}

// StartBackgroundLoops starts periodic flushing, idle-age S3 sweeping, and pending-recompose retries.
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

		reconciled := false
		if maxIdle > 0 {
			idx.SweepExpired(ctx, storage, registry, maxIdle)
			reconciled = true
		} else if err := idx.Reconcile(ctx, storage); err != nil && loopCtx.Err() == nil {
			log.Printf("[S3Index] Initial reconcile failed: %v", err)
		} else {
			reconciled = true
		}
		if cfg != nil {
			if s3MaxBytes := cfg.GetS3MaxBytes(); s3MaxBytes > 0 {
				idx.SweepS3OverCapacity(ctx, storage, registry, s3MaxBytes, reconciled)
			}
		}
		sweepStale(ctx)
		idx.TriggerPendingRecompose()
	}()

	// Periodic & recovery-triggered pending-recompose retry worker
	if extractS3Backend(storage) != nil {
		idx.wg.Add(1)
		go func() {
			defer idx.wg.Done()
			retryTicker := time.NewTicker(60 * time.Second)
			defer retryTicker.Stop()
			for {
				select {
				case <-idx.stopCh:
					return
				case <-idx.recomposeTrigger:
					recompTimeout := 30 * time.Minute
					if cfg != nil && cfg.GetS3Config().RecomposeTimeout > 0 {
						recompTimeout = cfg.GetS3Config().RecomposeTimeout
					}
					ctx, cancel := context.WithTimeout(loopCtx, recompTimeout)
					idx.ProcessPendingRecomposes(ctx, storage, registry, true)
					cancel()
				case <-retryTicker.C:
					recompTimeout := 30 * time.Minute
					if cfg != nil && cfg.GetS3Config().RecomposeTimeout > 0 {
						recompTimeout = cfg.GetS3Config().RecomposeTimeout
					}
					ctx, cancel := context.WithTimeout(loopCtx, recompTimeout)
					idx.ProcessPendingRecomposes(ctx, storage, registry, false)
					cancel()
				}
			}
		}()
	}

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

	// Periodic idle-age sweep + S3_MAX_BYTES LRU capacity enforcement
	idx.wg.Add(1)
	go func() {
		defer idx.wg.Done()
		evictTicker := time.NewTicker(interval)
		defer evictTicker.Stop()
		for {
			select {
			case <-idx.stopCh:
				return
			case <-idx.sizeEvictTrigger:
				var curS3MaxBytes int64
				if cfg != nil {
					curS3MaxBytes = cfg.GetS3MaxBytes()
				}
				if curS3MaxBytes > 0 {
					_, _, _, s3Bytes, _ := idx.StatsByTier()
					if s3Bytes > curS3MaxBytes {
						ctx, cancel := context.WithTimeout(loopCtx, 5*time.Minute)
						idx.SweepS3OverCapacity(ctx, storage, registry, curS3MaxBytes, true)
						cancel()
					}
				}
			case <-evictTicker.C:
				curMaxIdle := 720 * time.Hour
				var curS3MaxBytes int64
				if cfg != nil {
					curMaxIdle = cfg.GetCacheMaxIdleAge()
					curS3MaxBytes = cfg.GetS3MaxBytes()
				}
				ctx, cancel := context.WithTimeout(loopCtx, 5*time.Minute)
				if curMaxIdle > 0 {
					idx.SweepExpired(ctx, storage, registry, curMaxIdle)
				}
				if curS3MaxBytes > 0 {
					idx.SweepS3OverCapacity(ctx, storage, registry, curS3MaxBytes, curMaxIdle > 0)
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
