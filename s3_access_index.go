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

// S3IndexEntry stores persistent metadata for a single committed vod/<key> object in S3.
type S3IndexEntry struct {
	Key          string `json:"key"`
	URL          string `json:"url,omitempty"`
	Size         int64  `json:"size"`
	CreatedAt    int64  `json:"created_at"`
	LastAccessed int64  `json:"last_accessed"`
}

// S3AccessIndex tracks local aging metadata for S3-cached assets and evicts idle objects.
type S3AccessIndex struct {
	mu        sync.Mutex
	saveMu    sync.Mutex
	indexPath string
	entries   map[string]*S3IndexEntry
	dirty     bool
	nowFn      func() time.Time
	stopCh     chan struct{}
	stopOnce   sync.Once
	loopCancel context.CancelFunc
	wg         sync.WaitGroup
}

// GlobalS3Index tracks cached S3 assets when running with STORAGE_BACKEND=s3.
var GlobalS3Index *S3AccessIndex

// NewS3AccessIndex constructs an S3AccessIndex persisted at indexPath.
func NewS3AccessIndex(indexPath string) *S3AccessIndex {
	return &S3AccessIndex{
		indexPath: indexPath,
		entries:   make(map[string]*S3IndexEntry),
		nowFn:     time.Now,
		stopCh:    make(chan struct{}),
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

// RecordCommit records or updates an object newly committed to S3 and flushes immediately.
func (idx *S3AccessIndex) RecordCommit(key, targetURL string, size int64) {
	if idx == nil || key == "" {
		return
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if !ok {
		e = &S3IndexEntry{
			Key:       key,
			CreatedAt: nowUnix,
		}
		idx.entries[key] = e
	}
	if targetURL != "" {
		e.URL = targetURL
	}
	if size > 0 {
		e.Size = size
	}
	if e.CreatedAt == 0 {
		e.CreatedAt = nowUnix
	}
	e.LastAccessed = nowUnix
	idx.dirty = true
	idx.mu.Unlock()

	_ = idx.Flush()
}

// RecordAccess refreshes LastAccessed on real playback (inserting if missing from the index).
func (idx *S3AccessIndex) RecordAccess(key, targetURL string, size int64) {
	if idx == nil || key == "" {
		return
	}
	nowUnix := idx.now().Unix()

	idx.mu.Lock()
	e, ok := idx.entries[key]
	if !ok {
		e = &S3IndexEntry{
			Key:       key,
			CreatedAt: nowUnix,
		}
		idx.entries[key] = e
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

// Reconcile synchronizes the local index with actual objects in S3 vod/.
func (idx *S3AccessIndex) Reconcile(ctx context.Context, storage StorageBackend) error {
	if idx == nil || storage == nil {
		return nil
	}

	objs, err := storage.List(ctx)
	if err != nil {
		return err
	}

	nowUnix := idx.now().Unix()
	live := make(map[string]StorageObjectInfo, len(objs))
	for _, obj := range objs {
		live[obj.Key] = obj
	}

	idx.mu.Lock()
	added, removed, updated := 0, 0, 0
	for key, obj := range live {
		modUnix := nowUnix
		if !obj.LastModified.IsZero() {
			modUnix = obj.LastModified.Unix()
		}
		e, ok := idx.entries[key]
		if !ok {
			idx.entries[key] = &S3IndexEntry{
				Key:          key,
				Size:         obj.Size,
				CreatedAt:    modUnix,
				LastAccessed: modUnix,
			}
			added++
		} else if obj.Size > 0 && e.Size != obj.Size {
			e.Size = obj.Size
			updated++
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
		log.Printf("[S3Index] Reconciled with S3: tracked=%d (added=%d, pruned=%d, updated=%d)", totalCount, added, removed, updated)
	}
	return nil
}

// SweepExpired evicts S3 objects whose LastAccessed exceeds maxIdleAge and are not actively in use.
func (idx *S3AccessIndex) SweepExpired(ctx context.Context, storage StorageBackend, registry *CacheRegistry, maxIdleAge time.Duration) (int, int64) {
	if idx == nil || storage == nil || maxIdleAge <= 0 {
		return 0, 0
	}

	if err := idx.Reconcile(ctx, storage); err != nil {
		log.Printf("[S3Evictor] Skipping sweep: S3 reconcile failed: %v", err)
		return 0, 0
	}

	now := idx.now()

	idx.mu.Lock()
	var candidates []S3IndexEntry
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
		idx.mu.Unlock()
		if !stillExpired {
			continue
		}

		if err := storage.Delete(ctx, cand.Key); err != nil {
			log.Printf("[S3Evictor] Failed to delete %s from S3: %v", cand.Key, err)
			continue
		}

		idx.mu.Lock()
		delete(idx.entries, cand.Key)
		idx.dirty = true
		idx.mu.Unlock()

		idle := idx.now().Sub(time.Unix(cand.LastAccessed, 0)).Round(time.Second)
		evicted++
		freedBytes += cand.Size
		log.Printf("[S3Evictor] Evicted idle S3 object: %s (Size: %.2f MB, Idle: %s)",
			cand.Key, float64(cand.Size)/(1024*1024), idle)
	}

	if evicted > 0 {
		_ = idx.Flush()
		log.Printf("[S3Evictor] Sweep complete. Evicted %d objects, freed %.2f MB",
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
