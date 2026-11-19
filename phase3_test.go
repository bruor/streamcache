package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhase3ConfigHybridWatermarksAndEagerBackup(t *testing.T) {
	clearPhase0ConfigEnv(t)
	origCfg := GlobalConfig
	t.Cleanup(func() { GlobalConfig = origCfg })

	// 1. Defaults
	LoadConfig()
	cfg := GlobalConfig
	if cfg.GetHybridDemoteHighWatermark() != 80 {
		t.Errorf("Expected default HYBRID_DEMOTE_HIGH_WATERMARK=80, got %d", cfg.GetHybridDemoteHighWatermark())
	}
	if cfg.GetHybridDemoteLowWatermark() != 70 {
		t.Errorf("Expected default HYBRID_DEMOTE_LOW_WATERMARK=70, got %d", cfg.GetHybridDemoteLowWatermark())
	}
	if cfg.GetHybridEagerS3Backup() {
		t.Errorf("Expected default HYBRID_EAGER_S3_BACKUP=false, got true")
	}

	// 2. Custom valid values
	t.Setenv("HYBRID_DEMOTE_HIGH_WATERMARK", "85")
	t.Setenv("HYBRID_DEMOTE_LOW_WATERMARK", "65")
	t.Setenv("HYBRID_EAGER_S3_BACKUP", "true")
	LoadConfig()
	cfg = GlobalConfig
	if cfg.GetHybridDemoteHighWatermark() != 85 {
		t.Errorf("Expected custom HYBRID_DEMOTE_HIGH_WATERMARK=85, got %d", cfg.GetHybridDemoteHighWatermark())
	}
	if cfg.GetHybridDemoteLowWatermark() != 65 {
		t.Errorf("Expected custom HYBRID_DEMOTE_LOW_WATERMARK=65, got %d", cfg.GetHybridDemoteLowWatermark())
	}
	if !cfg.GetHybridEagerS3Backup() {
		t.Errorf("Expected custom HYBRID_EAGER_S3_BACKUP=true, got false")
	}

	// 3. Clamping > 100 and < 0, plus low <= high
	t.Setenv("HYBRID_DEMOTE_HIGH_WATERMARK", "120")
	t.Setenv("HYBRID_DEMOTE_LOW_WATERMARK", "95")
	LoadConfig()
	cfg = GlobalConfig
	if cfg.GetHybridDemoteHighWatermark() != 100 || cfg.GetHybridDemoteLowWatermark() != 95 {
		t.Errorf("Expected clamped high=100, low=95; got high=%d, low=%d",
			cfg.GetHybridDemoteHighWatermark(), cfg.GetHybridDemoteLowWatermark())
	}

	t.Setenv("HYBRID_DEMOTE_HIGH_WATERMARK", "60")
	t.Setenv("HYBRID_DEMOTE_LOW_WATERMARK", "95")
	LoadConfig()
	cfg = GlobalConfig
	if cfg.GetHybridDemoteHighWatermark() != 60 || cfg.GetHybridDemoteLowWatermark() != 60 {
		t.Errorf("Expected low clamped to high=60; got high=%d, low=%d",
			cfg.GetHybridDemoteHighWatermark(), cfg.GetHybridDemoteLowWatermark())
	}

	clearPhase0ConfigEnv(t)
	t.Setenv("HYBRID_DEMOTE_HIGH_WATERMARK", "50")
	LoadConfig()
	cfg = GlobalConfig
	if cfg.GetHybridDemoteHighWatermark() != 50 || cfg.GetHybridDemoteLowWatermark() != 50 {
		t.Errorf("Expected unset low watermark (default 70) clamped to high=50; got high=%d, low=%d",
			cfg.GetHybridDemoteHighWatermark(), cfg.GetHybridDemoteLowWatermark())
	}

	t.Setenv("HYBRID_DEMOTE_HIGH_WATERMARK", "-5")
	t.Setenv("HYBRID_DEMOTE_LOW_WATERMARK", "-10")
	LoadConfig()
	cfg = GlobalConfig
	if cfg.GetHybridDemoteHighWatermark() != 80 || cfg.GetHybridDemoteLowWatermark() != 70 {
		t.Errorf("Expected negative watermarks to fall back to defaults high=80, low=70; got high=%d, low=%d",
			cfg.GetHybridDemoteHighWatermark(), cfg.GetHybridDemoteLowWatermark())
	}
}

func TestPhase3RevalidateZeroCopyObjectAndSingleflight(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	const bucket = "test-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	var copyCount atomic.Int32
	mockS3.RequestHook = func(r *http.Request) int {
		if r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "" {
			copyCount.Add(1)
		}
		return 0
	}

	var probeCount atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount.Add(1)
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("ETag", `"etag-v1"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer origin.Close()

	tmpBase := t.TempDir()
	stateDir := filepath.Join(tmpBase, "state")
	_ = os.MkdirAll(stateDir, 0755)

	GlobalConfig = &Config{
		StorageBackend:          "s3",
		TmpDir:                  filepath.Join(tmpBase, "tmp"),
		StateDir:                stateDir,
		CacheRevalidateInterval: 1 * time.Hour,
		MaxUpstreamRetries:      2,
	}
	SetStorageTopology(InitStorageTopology(GlobalConfig))

	s3Cfg := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       bucket,
		Region:       "us-east-1",
		AccessKey:    "test",
		SecretKey:    "test",
		UsePathStyle: true,
		DisableSSL:   true,
	}
	s3Backend, err := NewS3StorageBackend(s3Cfg)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}
	GlobalStorage = s3Backend
	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
		stopCh:       make(chan struct{}),
	}

	idxPath := filepath.Join(stateDir, "cache_index.json")
	idx := NewCacheIndex(idxPath)
	GlobalCacheIndex = idx
	GlobalS3Index = idx

	key := "ep_singleflight.mp4"
	payload := bytes.Repeat([]byte("S"), 4096)
	localSrc := filepath.Join(tmpBase, "src.mp4")
	if err := os.WriteFile(localSrc, payload, 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	expiredTime := time.Now().Add(-2 * time.Hour).Unix()
	targetURL := origin.URL + "/" + key
	if err := s3Backend.CommitLocalFile(context.Background(), key, localSrc, FileMeta{
		URL:           targetURL,
		TotalSize:     int64(len(payload)),
		ContentType:   "video/mp4",
		ETag:          `"etag-v1"`,
		LastValidated: expiredTime,
		Completed:     true,
	}); err != nil {
		t.Fatalf("CommitLocalFile error: %v", err)
	}

	oldAccessed := time.Now().Add(-48 * time.Hour).Unix()
	idx.mu.Lock()
	idx.entries[key] = &CacheIndexEntry{
		Key:           key,
		URL:           targetURL,
		Size:          int64(len(payload)),
		Tier:          IndexTierS3,
		ContentType:   "video/mp4",
		ETag:          `"etag-v1"`,
		CreatedAt:     oldAccessed,
		LastValidated: expiredTime,
		LastAccessed:  oldAccessed,
	}
	idx.dirty = true
	idx.mu.Unlock()
	_ = idx.Flush()

	const workers = 8
	startCh := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, workers)
	valids := make([]bool, workers)
	metas := make([]*FileMeta, workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startCh
			v, m, rerr := RevalidateWithMeta(key, targetURL, nil, nil, TierS3)
			valids[i] = v
			metas[i] = m
			errs[i] = rerr
		}(i)
	}
	close(startCh)
	wg.Wait()

	for i := 0; i < workers; i++ {
		if errs[i] != nil || !valids[i] || metas[i] == nil {
			t.Fatalf("worker %d failed: valid=%v, err=%v, meta=%+v", i, valids[i], errs[i], metas[i])
		}
		if metas[i].LastValidated <= expiredTime {
			t.Errorf("worker %d expected refreshed LastValidated > %d, got %d", i, expiredTime, metas[i].LastValidated)
		}
	}

	if got := probeCount.Load(); got != 1 {
		t.Errorf("Expected singleflight Revalidate to trigger exactly 1 origin probe across %d concurrent callers, got %d", workers, got)
	}
	if got := copyCount.Load(); got != 0 {
		t.Errorf("Expected zero S3 CopyObject requests on metadata revalidation, got %d", got)
	}

	entry, ok := idx.Get(key)
	if !ok {
		t.Fatalf("Expected %s to remain tracked in CacheIndex", key)
	}
	if entry.LastAccessed != oldAccessed {
		t.Errorf("Expected RecordValidation NOT to overwrite LastAccessed (%d), got %d", oldAccessed, entry.LastAccessed)
	}
	if entry.LastValidated <= expiredTime {
		t.Errorf("Expected CacheIndex LastValidated to advance beyond %d, got %d", expiredTime, entry.LastValidated)
	}

	// Simulate process restart: fresh CacheIndex loaded from disk + fresh S3StorageBackend (empty metaCache)
	_ = idx.Flush()
	reloadedIdx := NewCacheIndex(idxPath)
	if err := reloadedIdx.Load(); err != nil {
		t.Fatalf("reloadedIdx.Load error: %v", err)
	}
	GlobalCacheIndex = reloadedIdx
	GlobalS3Index = reloadedIdx

	freshS3, err := NewS3StorageBackend(s3Cfg)
	if err != nil {
		t.Fatalf("fresh NewS3StorageBackend error: %v", err)
	}
	GlobalStorage = freshS3

	stMeta, stTier, stExists, stErr := freshS3.StatWithTier(context.Background(), key)
	if stErr != nil || !stExists || stTier != TierS3 || stMeta == nil {
		t.Fatalf("Expected StatWithTier hit on fresh S3 backend, got exists=%v tier=%s err=%v", stExists, stTier, stErr)
	}
	if stMeta.LastValidated <= expiredTime {
		t.Errorf("Expected fresh S3 backend StatWithTier to overlay persisted LastValidated > %d, got %d", expiredTime, stMeta.LastValidated)
	}

	valid2, _, err2 := RevalidateWithMeta(key, targetURL, nil, nil, TierS3)
	if err2 != nil || !valid2 {
		t.Fatalf("Expected post-restart Revalidate to succeed, got valid=%v err=%v", valid2, err2)
	}
	if got := probeCount.Load(); got != 1 {
		t.Errorf("Expected 0 additional origin probes after restart with persisted LastValidated, total probes=%d", got)
	}
}

func TestPhase3UnifiedCacheIndexReconcileAndIdleEvictionBothTiers(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	ctx := context.Background()

	// Part 1: Verify InitRegistry migrates STATE_DIR/s3_index.json -> STATE_DIR/cache_index.json
	migBase := t.TempDir()
	migCache := filepath.Join(migBase, "cache")
	migTmp := filepath.Join(migBase, "tmp")
	migState := filepath.Join(migBase, "state")
	_ = os.MkdirAll(migCache, 0755)
	_ = os.MkdirAll(migTmp, 0755)
	_ = os.MkdirAll(migState, 0755)

	_ = os.WriteFile(filepath.Join(migCache, "legacy.mkv"), []byte("legacy-data"), 0644)
	legacyIdx := NewS3AccessIndex(filepath.Join(migState, "s3_index.json"))
	legacyIdx.RecordCommit("legacy.mkv", "http://origin/legacy.mkv", 11)
	legacyIdx.Stop()

	GlobalConfig = &Config{
		StorageBackend:          "disk",
		CacheDir:                migCache,
		TmpDir:                  migTmp,
		StateDir:                migState,
		CacheMaxBytes:           10 * 1024 * 1024,
		CacheMaxItemPct:         50,
		CacheRevalidateInterval: 1 * time.Hour,
		CacheMaxIdleAge:         0, // Disable auto idle sweep on startup
	}
	GlobalStorage = nil
	SetStorageTopology(InitStorageTopology(GlobalConfig))
	if err := InitStorage(); err != nil {
		t.Fatalf("InitStorage disk error: %v", err)
	}
	InitRegistry()

	migratedPath := filepath.Join(migState, "cache_index.json")
	if !FileExists(migratedPath) {
		t.Fatalf("Expected InitRegistry to migrate STATE_DIR/s3_index.json -> %s", migratedPath)
	}
	if GlobalCacheIndex == nil {
		t.Fatalf("Expected GlobalCacheIndex to be initialized in disk mode")
	}
	if entry, ok := GlobalCacheIndex.Get("legacy.mkv"); !ok || entry.Size != 11 {
		t.Errorf("Expected migrated entry legacy.mkv in GlobalCacheIndex, got ok=%v entry=%+v", ok, entry)
	}

	// Part 2: Unified Reconcile + SweepExpired across disk, s3, and both tiers
	const bucket = "unified-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	baseDir := t.TempDir()
	localDir := filepath.Join(baseDir, "cache")
	tmpDir := filepath.Join(baseDir, "tmp")
	stateDir := filepath.Join(baseDir, "state")
	_ = os.MkdirAll(stateDir, 0755)

	localBackend := NewLocalStorageBackend(localDir, tmpDir)
	s3Backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       bucket,
		Region:       "us-east-1",
		AccessKey:    "test",
		SecretKey:    "test",
		UsePathStyle: true,
		DisableSSL:   true,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}
	tiered := NewTieredStorageBackend(localBackend, s3Backend)

	writeLocal := func(key string, size int) {
		p := localBackend.FilePath(key)
		if err := os.WriteFile(p, bytes.Repeat([]byte("D"), size), 0644); err != nil {
			t.Fatalf("write local %s: %v", key, err)
		}
		metaBytes, _ := json.Marshal(FileMeta{
			URL:         "http://origin/" + key,
			TotalSize:   int64(size),
			ContentType: "video/mp4",
			Completed:   true,
		})
		_ = os.WriteFile(p+".json", metaBytes, 0644)
	}
	writeS3 := func(key string, size int) {
		src := filepath.Join(tmpDir, key+".src")
		_ = os.WriteFile(src, bytes.Repeat([]byte("S"), size), 0644)
		if err := s3Backend.CommitLocalFile(ctx, key, src, FileMeta{
			URL:         "http://origin/" + key,
			TotalSize:   int64(size),
			ContentType: "video/mp4",
			Completed:   true,
		}); err != nil {
			t.Fatalf("CommitLocalFile S3 %s: %v", key, err)
		}
	}

	writeLocal("disk_old.mp4", 100)
	writeS3("s3_old.mp4", 200)
	writeLocal("both_old.mp4", 300)
	writeS3("both_old.mp4", 300)
	writeLocal("disk_active.mp4", 400)
	writeLocal("disk_fresh.mp4", 500)

	baseNow := time.Now()
	curNow := baseNow
	idx := NewCacheIndex(filepath.Join(stateDir, "cache_index.json"))
	idx.nowFn = func() time.Time { return curNow }

	if err := idx.Reconcile(ctx, tiered); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	expectedTiers := map[string]string{
		"disk_old.mp4":    IndexTierDisk,
		"s3_old.mp4":      IndexTierS3,
		"both_old.mp4":    IndexTierBoth,
		"disk_active.mp4": IndexTierDisk,
		"disk_fresh.mp4":  IndexTierDisk,
	}
	for k, want := range expectedTiers {
		if got := idx.GetTier(k); got != want {
			t.Errorf("key %s expected tier %q, got %q", k, want, got)
		}
	}

	// diskCnt counts all entries on disk (disk_old + disk_active + disk_fresh + both_old = 4, 1300B);
	// s3Cnt counts all entries on S3 (s3_old + both_old = 2, 500B); bothCnt counts entries on both (1).
	diskCnt, diskBytes, s3Cnt, s3Bytes, bothCnt := idx.StatsByTier()
	if diskCnt != 4 || diskBytes != 1300 || s3Cnt != 2 || s3Bytes != 500 || bothCnt != 1 {
		t.Errorf("Unexpected StatsByTier: disk=(%d,%d) s3=(%d,%d) both=%d",
			diskCnt, diskBytes, s3Cnt, s3Bytes, bothCnt)
	}
	if !mockS3.HasObject(bucket, s3Backend.monolithicKey("s3_old.mp4")) || !mockS3.HasObject(bucket, s3Backend.monolithicKey("both_old.mp4")) {
		t.Fatalf("Expected s3_old.mp4 and both_old.mp4 to exist in mock S3 before SweepExpired")
	}

	// Touch only disk_fresh.mp4 at day 39; sweep at day 40 with maxIdle=30d
	curNow = baseNow.Add(39 * 24 * time.Hour)
	idx.RecordAccessForTier("disk_fresh.mp4", "http://origin/disk_fresh.mp4", 500, IndexTierDisk)
	curNow = baseNow.Add(40 * 24 * time.Hour)

	reg := &CacheRegistry{
		Files: make(map[string]*FileCacheState),
		Connections: map[string]*ClientConnection{
			"conn-active": {ID: "conn-active", Key: "disk_active.mp4"},
		},
		TotalCacheSize: 100 + 300 + 400 + 500, // local disk bytes = 1300
	}

	evicted, freed := idx.SweepExpired(ctx, tiered, reg, 30*24*time.Hour)
	if evicted != 3 || freed != 600 {
		t.Errorf("Expected 3 expired objects freed (600B), got evicted=%d freed=%d", evicted, freed)
	}

	// Expired objects removed from every tier that held them and from idx
	if _, exists, _ := localBackend.Stat(ctx, "disk_old.mp4"); exists {
		t.Errorf("Expected disk_old.mp4 deleted from local disk")
	}
	if mockS3.HasObject(bucket, s3Backend.monolithicKey("s3_old.mp4")) {
		t.Errorf("Expected s3_old.mp4 deleted from S3")
	}
	if _, exists, _ := localBackend.Stat(ctx, "both_old.mp4"); exists || mockS3.HasObject(bucket, s3Backend.monolithicKey("both_old.mp4")) {
		t.Errorf("Expected both_old.mp4 deleted from BOTH local disk and S3")
	}
	for _, k := range []string{"disk_old.mp4", "s3_old.mp4", "both_old.mp4"} {
		if _, ok := idx.Get(k); ok {
			t.Errorf("Expected %s removed from CacheIndex after SweepExpired", k)
		}
	}

	// Active & fresh survive
	if _, exists, _ := localBackend.Stat(ctx, "disk_active.mp4"); !exists {
		t.Errorf("Expected active disk_active.mp4 to survive SweepExpired")
	}
	if _, exists, _ := localBackend.Stat(ctx, "disk_fresh.mp4"); !exists {
		t.Errorf("Expected fresh disk_fresh.mp4 to survive SweepExpired")
	}
	// TotalCacheSize decremented only by local objects (100 + 300 = 400 -> 900 remaining), never by s3_old (200)
	if got := reg.GetTotalCacheSize(); got != 900 {
		t.Errorf("Expected TotalCacheSize=900 after local+both eviction, got %d", got)
	}
}

func TestPhase3WatermarkDemotionEagerBackupAndPreEvictDemoteFirst(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	ctx := context.Background()

	const bucket = "hybrid-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	var failS3 atomic.Bool
	var s3PutCount atomic.Int32
	mockS3.RequestHook = func(r *http.Request) int {
		if failS3.Load() && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
			return http.StatusServiceUnavailable
		}
		if r.Method == http.MethodPut && r.URL.Query().Get("partNumber") == "" {
			s3PutCount.Add(1)
		}
		return 0
	}

	s3Cfg := S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       bucket,
		Region:       "us-east-1",
		AccessKey:    "test",
		SecretKey:    "test",
		UsePathStyle: true,
		DisableSSL:   true,
	}

	seedLocalFile := func(local *LocalStorageBackend, key string, size int, modTime time.Time) FileMeta {
		p := local.FilePath(key)
		if err := os.WriteFile(p, bytes.Repeat([]byte("H"), size), 0644); err != nil {
			t.Fatalf("seedLocalFile %s: %v", key, err)
		}
		meta := FileMeta{
			URL:           "http://origin/" + key,
			TotalSize:     int64(size),
			ContentType:   "video/mp4",
			LastValidated: modTime.Unix(),
			Completed:     true,
		}
		metaBytes, _ := json.Marshal(meta)
		_ = os.WriteFile(p+".json", metaBytes, 0644)
		_ = os.Chtimes(p, modTime, modTime)
		return meta
	}

	// Subtest A: Watermark demotion 80% -> 70%, AlreadyOnS3 first, active skipped, LRU ordering
	t.Run("WatermarkDemotion_80To70", func(t *testing.T) {
		base := t.TempDir()
		localDir := filepath.Join(base, "cache")
		tmpDir := filepath.Join(base, "tmp")
		stateDir := filepath.Join(base, "state")
		_ = os.MkdirAll(stateDir, 0755)

		local := NewLocalStorageBackend(localDir, tmpDir)
		s3Backend, err := NewS3StorageBackend(s3Cfg)
		if err != nil {
			t.Fatalf("NewS3StorageBackend error: %v", err)
		}
		tiered := NewTieredStorageBackend(local, s3Backend)

		cfg := &Config{
			StorageBackend:             "hybrid",
			CacheDir:                   localDir,
			TmpDir:                     tmpDir,
			StateDir:                   stateDir,
			CacheMaxBytes:              1000,
			HybridDemoteHighWatermark:  80,
			HybridDemoteLowWatermark:   70,
			CacheRevalidateInterval:    1 * time.Hour,
		}
		reg := &CacheRegistry{
			Files: make(map[string]*FileCacheState),
			Connections: map[string]*ClientConnection{
				"active": {ID: "active", Key: "ep_active_oldest.mp4"},
			},
		}
		idx := NewCacheIndex(filepath.Join(stateDir, "cache_index.json"))

		now := time.Now()
		seedIdx := func(key string, meta FileMeta, tier string, accessTime time.Time) {
			idx.mu.Lock()
			idx.entries[key] = &CacheIndexEntry{
				Key:           key,
				URL:           meta.URL,
				Size:          meta.TotalSize,
				Tier:          tier,
				ContentType:   meta.ContentType,
				CreatedAt:     accessTime.Unix(),
				LastValidated: meta.LastValidated,
				LastAccessed:  accessTime.Unix(),
			}
			idx.mu.Unlock()
		}

		// 5 files * 180B = 900B (90% >= 80% high watermark 800B; target <= 700B)
		mBoth := seedLocalFile(local, "ep_both.mp4", 180, now.Add(-1*time.Hour))
		if err := s3Backend.UploadFromFile(ctx, "ep_both.mp4", local.FilePath("ep_both.mp4"), mBoth); err != nil {
			t.Fatalf("seed S3 ep_both: %v", err)
		}
		seedIdx("ep_both.mp4", mBoth, IndexTierBoth, now.Add(-1*time.Hour))

		mAct := seedLocalFile(local, "ep_active_oldest.mp4", 180, now.Add(-10*time.Hour))
		seedIdx("ep_active_oldest.mp4", mAct, IndexTierDisk, now.Add(-10*time.Hour))

		mOld := seedLocalFile(local, "ep_disk_old.mp4", 180, now.Add(-5*time.Hour))
		seedIdx("ep_disk_old.mp4", mOld, IndexTierDisk, now.Add(-5*time.Hour))

		mNew1 := seedLocalFile(local, "ep_disk_new1.mp4", 180, now.Add(-2*time.Hour))
		seedIdx("ep_disk_new1.mp4", mNew1, IndexTierDisk, now.Add(-2*time.Hour))

		mNew2 := seedLocalFile(local, "ep_disk_new2.mp4", 180, now.Add(-1*time.Hour))
		seedIdx("ep_disk_new2.mp4", mNew2, IndexTierDisk, now.Add(-1*time.Hour))

		reg.TotalCacheSize = 900
		putsBefore := s3PutCount.Load()

		demoter := NewHybridDemoter(tiered, idx, reg, cfg)
		defer demoter.Stop()

		demoted, freed, derr := demoter.RunDemotionPass(ctx)
		if derr != nil {
			t.Fatalf("RunDemotionPass error: %v", derr)
		}
		if demoted != 2 || freed != 360 {
			t.Errorf("Expected 2 demotions freeing 360B (900->540<=700), got demoted=%d freed=%d", demoted, freed)
		}

		// ep_both freed locally without any S3 PUT; ep_disk_old uploaded (1 PUT) then freed locally
		if puts := s3PutCount.Load() - putsBefore; puts != 1 {
			t.Errorf("Expected exactly 1 S3 PUT (only ep_disk_old; ep_both was already on S3), got %d", puts)
		}
		if _, exists, _ := local.Stat(ctx, "ep_both.mp4"); exists {
			t.Errorf("Expected ep_both.mp4 removed from L1 local disk")
		}
		if !mockS3.HasObject(bucket, s3Backend.monolithicKey("ep_both.mp4")) || idx.GetTier("ep_both.mp4") != IndexTierS3 {
			t.Errorf("Expected ep_both.mp4 preserved on S3 with tier=s3, got tier=%s", idx.GetTier("ep_both.mp4"))
		}
		if _, exists, _ := local.Stat(ctx, "ep_disk_old.mp4"); exists {
			t.Errorf("Expected ep_disk_old.mp4 removed from L1 local disk")
		}
		if !mockS3.HasObject(bucket, s3Backend.monolithicKey("ep_disk_old.mp4")) || idx.GetTier("ep_disk_old.mp4") != IndexTierS3 {
			t.Errorf("Expected ep_disk_old.mp4 uploaded to S3 with tier=s3, got tier=%s", idx.GetTier("ep_disk_old.mp4"))
		}
		for _, keepKey := range []string{"ep_active_oldest.mp4", "ep_disk_new1.mp4", "ep_disk_new2.mp4"} {
			if _, exists, _ := local.Stat(ctx, keepKey); !exists {
				t.Errorf("Expected %s to remain on L1 local disk", keepKey)
			}
		}
		if got := reg.GetTotalCacheSize(); got != 540 {
			t.Errorf("Expected TotalCacheSize=540 after demotion, got %d", got)
		}
	})

	// Subtest B: S3-offline safety vs emergency PreEvict/evictInactiveFiles
	t.Run("S3OfflinePreservesLocalInDemoter_EvictStillFreesDisk", func(t *testing.T) {
		base := t.TempDir()
		localDir := filepath.Join(base, "cache")
		tmpDir := filepath.Join(base, "tmp")
		stateDir := filepath.Join(base, "state")
		_ = os.MkdirAll(stateDir, 0755)

		local := NewLocalStorageBackend(localDir, tmpDir)
		s3Backend, err := NewS3StorageBackend(s3Cfg)
		if err != nil {
			t.Fatalf("NewS3StorageBackend error: %v", err)
		}
		tiered := NewTieredStorageBackend(local, s3Backend)

		cfg := &Config{
			StorageBackend:            "hybrid",
			CacheDir:                  localDir,
			TmpDir:                    tmpDir,
			StateDir:                  stateDir,
			CacheMaxBytes:             1000,
			HybridDemoteHighWatermark: 80,
			HybridDemoteLowWatermark:  70,
			CacheRevalidateInterval:   1 * time.Hour,
		}
		reg := &CacheRegistry{
			Files:          make(map[string]*FileCacheState),
			Connections:    make(map[string]*ClientConnection),
			TotalCacheSize: 900,
		}
		idx := NewCacheIndex(filepath.Join(stateDir, "cache_index.json"))

		GlobalConfig = cfg
		GlobalStorage = tiered
		GlobalRegistry = reg
		GlobalCacheIndex = idx
		GlobalS3Index = idx

		now := time.Now()
		mOld := seedLocalFile(local, "old_unbacked.mp4", 450, now.Add(-5*time.Hour))
		mNew := seedLocalFile(local, "new_unbacked.mp4", 450, now.Add(-1*time.Hour))
		idx.RecordCommitForTier("old_unbacked.mp4", mOld, IndexTierDisk)
		idx.RecordCommitForTier("new_unbacked.mp4", mNew, IndexTierDisk)
		idx.mu.Lock()
		idx.entries["old_unbacked.mp4"].LastAccessed = now.Add(-5 * time.Hour).Unix()
		idx.entries["new_unbacked.mp4"].LastAccessed = now.Add(-1 * time.Hour).Unix()
		idx.mu.Unlock()

		failS3.Store(true)
		demoter := NewHybridDemoter(tiered, idx, reg, cfg)
		defer demoter.Stop()

		if _, _, err := demoter.RunDemotionPass(ctx); err == nil {
			t.Fatalf("Expected RunDemotionPass to return an error when S3 PUT returns 503")
		}
		if _, exists, _ := local.Stat(ctx, "old_unbacked.mp4"); !exists {
			t.Errorf("Expected old_unbacked.mp4 preserved on L1 when S3 fails during RunDemotionPass")
		}
		if _, exists, _ := local.Stat(ctx, "new_unbacked.mp4"); !exists {
			t.Errorf("Expected new_unbacked.mp4 preserved on L1 when S3 fails during RunDemotionPass")
		}

		// Emergency evictInactiveFiles while S3 is still 503 must STILL free L1 disk space
		reg.evictInactiveFiles(450)
		if _, exists, _ := local.Stat(ctx, "old_unbacked.mp4"); exists {
			t.Errorf("Expected evictInactiveFiles to remove oldest local file even when S3 is down")
		}
		if _, exists, _ := local.Stat(ctx, "new_unbacked.mp4"); !exists {
			t.Errorf("Expected newer file to remain after 450B eviction")
		}
		if got := reg.GetTotalCacheSize(); got != 450 {
			t.Errorf("Expected TotalCacheSize=450 after first eviction, got %d", got)
		}

		// Restore S3 and evict remaining file -> must demote to S3 BEFORE removing locally
		failS3.Store(false)
		reg.evictInactiveFiles(450)
		if _, exists, _ := local.Stat(ctx, "new_unbacked.mp4"); exists {
			t.Errorf("Expected new_unbacked.mp4 removed from local disk after second eviction")
		}
		if !mockS3.HasObject(bucket, s3Backend.monolithicKey("new_unbacked.mp4")) {
			t.Errorf("Expected evictInactiveFiles in hybrid mode to upload new_unbacked.mp4 to S3 before local deletion")
		}
		if got := idx.GetTier("new_unbacked.mp4"); got != IndexTierS3 {
			t.Errorf("Expected CacheIndex tier=s3 for demote-before-delete asset, got %q", got)
		}
		if got := reg.GetTotalCacheSize(); got != 0 {
			t.Errorf("Expected TotalCacheSize=0 after full local eviction, got %d", got)
		}
	})

	// Subtest C: HYBRID_EAGER_S3_BACKUP=true uploads async to S3 while keeping L1 warm (Tier=both)
	t.Run("EagerS3Backup", func(t *testing.T) {
		base := t.TempDir()
		localDir := filepath.Join(base, "cache")
		tmpDir := filepath.Join(base, "tmp")
		stateDir := filepath.Join(base, "state")
		_ = os.MkdirAll(stateDir, 0755)

		local := NewLocalStorageBackend(localDir, tmpDir)
		s3Backend, err := NewS3StorageBackend(s3Cfg)
		if err != nil {
			t.Fatalf("NewS3StorageBackend error: %v", err)
		}
		tiered := NewTieredStorageBackend(local, s3Backend)

		cfg := &Config{
			StorageBackend:            "hybrid",
			CacheDir:                  localDir,
			TmpDir:                    tmpDir,
			StateDir:                  stateDir,
			CacheMaxBytes:             100000,
			HybridDemoteHighWatermark: 80,
			HybridDemoteLowWatermark:  70,
			HybridEagerS3Backup:       true,
			CacheRevalidateInterval:   1 * time.Hour,
		}
		reg := &CacheRegistry{
			Files:          make(map[string]*FileCacheState),
			Connections:    make(map[string]*ClientConnection),
			TotalCacheSize: 256,
		}
		idx := NewCacheIndex(filepath.Join(stateDir, "cache_index.json"))

		meta := seedLocalFile(local, "eager.mp4", 256, time.Now())
		idx.RecordCommitForTier("eager.mp4", meta, IndexTierDisk)

		demoter := NewHybridDemoter(tiered, idx, reg, cfg)
		defer demoter.Stop()

		demoter.EnqueueEagerBackup("eager.mp4")

		ok := waitForPhase1Condition(5*time.Second, func() bool {
			return idx.GetTier("eager.mp4") == IndexTierBoth
		})
		if !ok {
			t.Fatalf("Timed out waiting for eager S3 backup; tier=%q", idx.GetTier("eager.mp4"))
		}
		if _, exists, _ := local.Stat(ctx, "eager.mp4"); !exists {
			t.Errorf("Expected L1 local copy of eager.mp4 to remain warm after eager S3 backup")
		}
		if !mockS3.HasObject(bucket, s3Backend.monolithicKey("eager.mp4")) {
			t.Errorf("Expected eager.mp4 to be uploaded to S3")
		}
		_ = fmt.Sprintf("verified eager S3 backup")
	})
}
