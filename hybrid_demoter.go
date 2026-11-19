package main

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// GlobalHybridDemoter manages asynchronous watermark-based L1->L2 demotion
// and optional eager L2 backup in hybrid storage mode.
var GlobalHybridDemoter *HybridDemoter

var globalDemoterMu sync.RWMutex

func getGlobalHybridDemoter() *HybridDemoter {
	globalDemoterMu.RLock()
	defer globalDemoterMu.RUnlock()
	return GlobalHybridDemoter
}

func setGlobalHybridDemoter(d *HybridDemoter) {
	globalDemoterMu.Lock()
	defer globalDemoterMu.Unlock()
	GlobalHybridDemoter = d
}

// HybridDemoter handles L1 (local disk) -> L2 (S3) watermark demotion
// and optional eager background S3 backup for freshly committed assets.
type HybridDemoter struct {
	storage  *TieredStorageBackend
	index    *CacheIndex
	registry *CacheRegistry
	cfg      *Config

	triggerCh     chan struct{}
	backupCh      chan string
	backupPending map[string]struct{}

	mu       sync.Mutex
	passMu   sync.Mutex
	stopOnce sync.Once
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// NewHybridDemoter creates and starts background workers for hybrid tier demotion and eager S3 backup.
func NewHybridDemoter(storage *TieredStorageBackend, index *CacheIndex, registry *CacheRegistry, cfg *Config) *HybridDemoter {
	ctx, cancel := context.WithCancel(context.Background())
	d := &HybridDemoter{
		storage:       storage,
		index:         index,
		registry:      registry,
		cfg:           cfg,
		triggerCh:     make(chan struct{}, 1),
		backupCh:      make(chan string, 256),
		backupPending: make(map[string]struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
	d.wg.Add(2)
	go d.watermarkWorker()
	go d.backupWorker()
	return d
}

// Stop signals the demoter workers to exit and waits for them to finish.
func (d *HybridDemoter) Stop() {
	if d == nil {
		return
	}
	d.stopOnce.Do(func() {
		d.cancel()
	})
	d.wg.Wait()
}

// TriggerWatermarkDemotion schedules an asynchronous watermark demotion check if one is not already queued.
func (d *HybridDemoter) TriggerWatermarkDemotion() {
	if d == nil {
		return
	}
	select {
	case <-d.ctx.Done():
		return
	default:
	}
	select {
	case d.triggerCh <- struct{}{}:
	default:
	}
}

// EnqueueEagerBackup queues a freshly committed L1 asset for asynchronous S3 backup when enabled.
func (d *HybridDemoter) EnqueueEagerBackup(key string) {
	if d == nil || key == "" {
		return
	}
	if d.cfg == nil || !d.cfg.GetHybridEagerS3Backup() {
		return
	}
	select {
	case <-d.ctx.Done():
		return
	default:
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.backupPending[key]; exists {
		return
	}
	select {
	case d.backupCh <- key:
		d.backupPending[key] = struct{}{}
	default:
		log.Printf("[HybridDemoter] Eager S3 backup queue full; skipping %s", key)
	}
}

func (d *HybridDemoter) watermarkWorker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-d.triggerCh:
			if _, _, err := d.RunDemotionPass(d.ctx); err != nil && d.ctx.Err() == nil {
				log.Printf("[HybridDemoter] Watermark demotion pass interrupted: %v", err)
			}
		}
	}
}

func (d *HybridDemoter) backupWorker() {
	defer d.wg.Done()
	for {
		select {
		case <-d.ctx.Done():
			return
		case key := <-d.backupCh:
			d.mu.Lock()
			delete(d.backupPending, key)
			d.mu.Unlock()

			d.runEagerBackupKey(d.ctx, key)
		}
	}
}

func (d *HybridDemoter) localFilePath(key string) string {
	if key == "" {
		return ""
	}
	if d.storage != nil && d.storage.Local() != nil {
		if p := d.storage.Local().FilePath(key); p != "" && FileExists(p) {
			return p
		}
	}
	if d.cfg != nil {
		if cacheDir := d.cfg.GetCacheDir(); cacheDir != "" {
			candidate := filepath.Join(cacheDir, key)
			if FileExists(candidate) {
				return candidate
			}
		}
	}
	return ""
}

func (d *HybridDemoter) runEagerBackupKey(ctx context.Context, key string) {
	if key == "" || d.storage == nil || d.storage.Local() == nil || d.storage.S3() == nil {
		return
	}
	if !IsNASHealthy() || !IsS3Healthy() {
		return
	}
	if d.index != nil {
		if tier := d.index.GetTier(key); tier == IndexTierBoth || tier == IndexTierS3 {
			return
		}
	}

	localMeta, exists, err := d.storage.Local().Stat(ctx, key)
	if err != nil || !exists || localMeta == nil {
		return
	}
	localPath := d.localFilePath(key)
	if localPath == "" {
		return
	}

	if err := d.storage.S3().UploadFromFile(ctx, key, localPath, *localMeta); err != nil {
		RecordS3Failure(err)
		log.Printf("[HybridDemoter] Eager S3 backup failed for %s (L1 retained): %v", key, err)
		return
	}
	RecordS3Success()

	if d.index != nil {
		if _, exists := d.index.Get(key); !exists {
			d.index.RecordCommitForTier(key, *localMeta, IndexTierBoth)
		} else {
			d.index.MarkAddedToTier(key, IndexTierS3)
		}
	}
	log.Printf("[HybridDemoter] Eager S3 backup complete for %s (Tier=both, %d bytes)", key, localMeta.TotalSize)
}

type demoteCandidate struct {
	Key          string
	Size         int64
	AlreadyOnS3  bool
	LastAccessed time.Time
}

// RunDemotionPass checks L1 local cache occupancy against HYBRID_DEMOTE_HIGH_WATERMARK
// and demotes least-recently-accessed inactive entries to L2 S3 until occupancy reaches
// or drops below HYBRID_DEMOTE_LOW_WATERMARK. If an S3 upload fails, the local file is
// preserved and the pass aborts so L1 never loses unbacked data.
func (d *HybridDemoter) RunDemotionPass(ctx context.Context) (int, int64, error) {
	if d == nil || d.storage == nil || d.storage.Local() == nil || d.storage.S3() == nil || d.cfg == nil {
		return 0, 0, nil
	}
	if !IsNASHealthy() || !IsS3Healthy() {
		return 0, 0, ErrStorageCircuitOpen
	}
	if ctx == nil {
		ctx = context.Background()
	}

	d.passMu.Lock()
	defer d.passMu.Unlock()

	maxBytes := d.cfg.GetCacheMaxBytes()
	highPct := d.cfg.GetHybridDemoteHighWatermark()
	lowPct := d.cfg.GetHybridDemoteLowWatermark()
	if maxBytes <= 0 || highPct <= 0 {
		return 0, 0, nil
	}
	if lowPct > highPct {
		lowPct = highPct
	}
	if lowPct < 0 {
		lowPct = 0
	}

	highBytes := int64(float64(maxBytes) * float64(highPct) / 100.0)
	lowBytes := int64(float64(maxBytes) * float64(lowPct) / 100.0)

	localObjs, err := d.storage.Local().List(ctx)
	if err != nil {
		return 0, 0, err
	}

	var localTotalBytes int64
	for _, obj := range localObjs {
		localTotalBytes += obj.Size
	}
	if localTotalBytes < highBytes {
		return 0, 0, nil
	}

	candidates := make([]demoteCandidate, 0, len(localObjs))
	for _, obj := range localObjs {
		if d.registry != nil && d.registry.IsKeyInUse(obj.Key) {
			continue
		}

		var alreadyOnS3 bool
		var lastAccessed time.Time
		if d.index != nil {
			if entry, ok := d.index.Get(obj.Key); ok {
				alreadyOnS3 = entry.Tier == IndexTierBoth || entry.Tier == IndexTierS3
				if entry.LastAccessed > 0 {
					lastAccessed = time.Unix(entry.LastAccessed, 0)
				}
			}
		}
		if lastAccessed.IsZero() {
			if meta, exists, statErr := d.storage.Local().Stat(ctx, obj.Key); statErr == nil && exists && meta != nil && meta.LastValidated > 0 {
				lastAccessed = time.Unix(meta.LastValidated, 0)
			}
		}
		if lastAccessed.IsZero() {
			lastAccessed = obj.LastModified
		}

		candidates = append(candidates, demoteCandidate{
			Key:          obj.Key,
			Size:         obj.Size,
			AlreadyOnS3:  alreadyOnS3,
			LastAccessed: lastAccessed,
		})
	}

	// Free already-backed Tier=="both" local copies first (zero S3 upload cost),
	// then oldest LastAccessed first.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].AlreadyOnS3 != candidates[j].AlreadyOnS3 {
			return candidates[i].AlreadyOnS3
		}
		if !candidates[i].LastAccessed.Equal(candidates[j].LastAccessed) {
			return candidates[i].LastAccessed.Before(candidates[j].LastAccessed)
		}
		return candidates[i].Key < candidates[j].Key
	})

	demoted := 0
	var freed int64
	for _, cand := range candidates {
		if localTotalBytes <= lowBytes {
			break
		}
		if err := ctx.Err(); err != nil {
			return demoted, freed, err
		}
		if d.registry != nil && d.registry.IsKeyInUse(cand.Key) {
			continue
		}

		alreadyOnS3 := cand.AlreadyOnS3
		var localMeta *FileMeta
		if !alreadyOnS3 {
			meta, exists, statErr := d.storage.Local().Stat(ctx, cand.Key)
			if statErr != nil || !exists || meta == nil {
				continue
			}
			localMeta = meta

			if _, existsInS3, s3StatErr := d.storage.S3().Stat(ctx, cand.Key); s3StatErr == nil && existsInS3 {
				alreadyOnS3 = true
				if d.index != nil {
					d.index.MarkAddedToTier(cand.Key, IndexTierS3)
				}
			}
		}

		if !alreadyOnS3 {
			localPath := d.localFilePath(cand.Key)
			if localPath == "" {
				continue
			}
			if uploadErr := d.storage.S3().UploadFromFile(ctx, cand.Key, localPath, *localMeta); uploadErr != nil {
				RecordS3Failure(uploadErr)
				log.Printf("[HybridDemoter] S3 upload failed for %s during demotion; preserving local L1 copy: %v", cand.Key, uploadErr)
				return demoted, freed, uploadErr
			}
			RecordS3Success()
			if d.index != nil {
				if _, exists := d.index.Get(cand.Key); !exists {
					d.index.RecordCommitForTier(cand.Key, *localMeta, IndexTierBoth)
				} else {
					d.index.MarkAddedToTier(cand.Key, IndexTierS3)
				}
			}
		}

		// Re-check active usage after upload in case a reader opened the file mid-upload.
		if d.registry != nil && d.registry.IsKeyInUse(cand.Key) {
			continue
		}

		if delErr := d.storage.DeleteLocal(ctx, cand.Key); delErr != nil {
			log.Printf("[HybridDemoter] Failed to remove demoted local L1 file %s: %v", cand.Key, delErr)
			continue
		}

		if d.registry != nil {
			d.registry.UpdateSize(-cand.Size)
		}
		if d.index != nil {
			d.index.MarkAddedToTier(cand.Key, IndexTierS3)
			d.index.MarkRemovedFromTier(cand.Key, IndexTierDisk)
		}

		localTotalBytes -= cand.Size
		demoted++
		freed += cand.Size
	}

	if demoted > 0 {
		log.Printf("[HybridDemoter] Demoted %d asset(s) (%d bytes) from L1 to L2 S3 (remaining L1: %d bytes, target <= %d bytes)", demoted, freed, localTotalBytes, lowBytes)
	}
	return demoted, freed, nil
}
