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

	// 1. Defaults: CACHE_MAX_ITEM_PCT=50 -> HYBRID_DEMOTE_THRESHOLD defaults to 100 - 50 = 50
	LoadConfig()
	cfg := GlobalConfig
	if cfg.GetHybridDemoteThreshold() != 50 {
		t.Errorf("Expected default HYBRID_DEMOTE_THRESHOLD=50 (100 - CACHE_MAX_ITEM_PCT=50), got %d", cfg.GetHybridDemoteThreshold())
	}
	if cfg.GetHybridEagerS3Backup() {
		t.Errorf("Expected default HYBRID_EAGER_S3_BACKUP=false, got true")
	}

	// 2. Inverse default tracks max(50, 100 - CACHE_MAX_ITEM_PCT) when 1..99, and 100 (reactive nudge) for 0/100
	t.Setenv("CACHE_MAX_ITEM_PCT", "20")
	LoadConfig()
	if got := GlobalConfig.GetHybridDemoteThreshold(); got != 80 {
		t.Errorf("Expected default HYBRID_DEMOTE_THRESHOLD=80 for CACHE_MAX_ITEM_PCT=20, got %d", got)
	}
	t.Setenv("CACHE_MAX_ITEM_PCT", "10")
	LoadConfig()
	if got := GlobalConfig.GetHybridDemoteThreshold(); got != 90 {
		t.Errorf("Expected default HYBRID_DEMOTE_THRESHOLD=90 for CACHE_MAX_ITEM_PCT=10, got %d", got)
	}
	t.Setenv("CACHE_MAX_ITEM_PCT", "80")
	LoadConfig()
	if got := GlobalConfig.GetHybridDemoteThreshold(); got != 50 {
		t.Errorf("Expected floored default HYBRID_DEMOTE_THRESHOLD=50 for CACHE_MAX_ITEM_PCT=80, got %d", got)
	}
	t.Setenv("CACHE_MAX_ITEM_PCT", "0")
	LoadConfig()
	if got := GlobalConfig.GetHybridDemoteThreshold(); got != 100 {
		t.Errorf("Expected fallback HYBRID_DEMOTE_THRESHOLD=100 for CACHE_MAX_ITEM_PCT=0, got %d", got)
	}
	t.Setenv("CACHE_MAX_ITEM_PCT", "100")
	LoadConfig()
	if got := GlobalConfig.GetHybridDemoteThreshold(); got != 100 {
		t.Errorf("Expected fallback HYBRID_DEMOTE_THRESHOLD=100 for CACHE_MAX_ITEM_PCT=100, got %d", got)
	}

	// 3. Explicit custom valid values override the inverse default
	t.Setenv("CACHE_MAX_ITEM_PCT", "20")
	t.Setenv("HYBRID_DEMOTE_THRESHOLD", "85")
	t.Setenv("HYBRID_EAGER_S3_BACKUP", "true")
	LoadConfig()
	cfg = GlobalConfig
	if cfg.GetHybridDemoteThreshold() != 85 {
		t.Errorf("Expected custom HYBRID_DEMOTE_THRESHOLD=85, got %d", cfg.GetHybridDemoteThreshold())
	}
	if !cfg.GetHybridEagerS3Backup() {
		t.Errorf("Expected custom HYBRID_EAGER_S3_BACKUP=true, got false")
	}

	t.Setenv("HYBRID_DEMOTE_THRESHOLD", "100")
	LoadConfig()
	if GlobalConfig.GetHybridDemoteThreshold() != 100 {
		t.Errorf("Expected explicit HYBRID_DEMOTE_THRESHOLD=100, got %d", GlobalConfig.GetHybridDemoteThreshold())
	}

	// 4. Clamping > 100 and < 0 fallback
	t.Setenv("HYBRID_DEMOTE_THRESHOLD", "120")
	LoadConfig()
	if GlobalConfig.GetHybridDemoteThreshold() != 100 {
		t.Errorf("Expected clamped threshold=100; got %d", GlobalConfig.GetHybridDemoteThreshold())
	}

	t.Setenv("CACHE_MAX_ITEM_PCT", "20")
	t.Setenv("HYBRID_DEMOTE_THRESHOLD", "-5")
	LoadConfig()
	if GlobalConfig.GetHybridDemoteThreshold() != 80 {
		t.Errorf("Expected negative threshold with CACHE_MAX_ITEM_PCT=20 to fall back to 80; got %d", GlobalConfig.GetHybridDemoteThreshold())
	}

	t.Setenv("CACHE_MAX_ITEM_PCT", "")
	t.Setenv("HYBRID_DEMOTE_THRESHOLD", "-5")
	LoadConfig()
	if GlobalConfig.GetHybridDemoteThreshold() != 50 {
		t.Errorf("Expected negative threshold with default CACHE_MAX_ITEM_PCT=50 to fall back to 50; got %d", GlobalConfig.GetHybridDemoteThreshold())
	}

	// 5. Startup warning only when hybrid mode sets HYBRID_DEMOTE_THRESHOLD < CACHE_MAX_ITEM_PCT
	if w := detectHybridThresholdWarning(&Config{
		StorageBackend:        "hybrid",
		CacheDir:              "./cache/files",
		CacheMaxItemPct:       50,
		HybridDemoteThreshold: 20,
		S3Config:              S3Config{Bucket: "test-bucket"},
	}); w == "" {
		t.Errorf("Expected warning when hybrid HYBRID_DEMOTE_THRESHOLD (20) < CACHE_MAX_ITEM_PCT (50)")
	}
	if w := detectHybridThresholdWarning(&Config{
		StorageBackend:        "hybrid",
		CacheDir:              "./cache/files",
		CacheMaxItemPct:       50,
		HybridDemoteThreshold: 50,
		S3Config:              S3Config{Bucket: "test-bucket"},
	}); w != "" {
		t.Errorf("Expected no warning when HYBRID_DEMOTE_THRESHOLD == CACHE_MAX_ITEM_PCT, got %q", w)
	}
	if w := detectHybridThresholdWarning(&Config{
		StorageBackend:        "disk",
		CacheDir:              "./cache/files",
		CacheMaxItemPct:       50,
		HybridDemoteThreshold: 20,
	}); w != "" {
		t.Errorf("Expected no warning in disk mode, got %q", w)
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
	t.Run("WatermarkDemotion_80", func(t *testing.T) {
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
			StorageBackend:          "hybrid",
			CacheDir:                localDir,
			TmpDir:                  tmpDir,
			StateDir:                stateDir,
			CacheMaxBytes:           1000,
			HybridDemoteThreshold:   80,
			CacheRevalidateInterval: 1 * time.Hour,
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

		// 5 files * 210B = 1050B (> 80% threshold 800B -> demotes 2 files to reach 630B <= 800B)
		mBoth := seedLocalFile(local, "ep_both.mp4", 210, now.Add(-1*time.Hour))
		if err := s3Backend.UploadFromFile(ctx, "ep_both.mp4", local.FilePath("ep_both.mp4"), mBoth); err != nil {
			t.Fatalf("seed S3 ep_both: %v", err)
		}
		seedIdx("ep_both.mp4", mBoth, IndexTierBoth, now.Add(-1*time.Hour))

		mAct := seedLocalFile(local, "ep_active_oldest.mp4", 210, now.Add(-10*time.Hour))
		seedIdx("ep_active_oldest.mp4", mAct, IndexTierDisk, now.Add(-10*time.Hour))

		mOld := seedLocalFile(local, "ep_disk_old.mp4", 210, now.Add(-5*time.Hour))
		seedIdx("ep_disk_old.mp4", mOld, IndexTierDisk, now.Add(-5*time.Hour))

		mNew1 := seedLocalFile(local, "ep_disk_new1.mp4", 210, now.Add(-2*time.Hour))
		seedIdx("ep_disk_new1.mp4", mNew1, IndexTierDisk, now.Add(-2*time.Hour))

		mNew2 := seedLocalFile(local, "ep_disk_new2.mp4", 210, now.Add(-1*time.Hour))
		seedIdx("ep_disk_new2.mp4", mNew2, IndexTierDisk, now.Add(-1*time.Hour))

		reg.TotalCacheSize = 1050
		putsBefore := s3PutCount.Load()

		demoter := NewHybridDemoter(tiered, idx, reg, cfg)
		defer demoter.Stop()

		demoted, freed, derr := demoter.RunDemotionPass(ctx)
		if derr != nil {
			t.Fatalf("RunDemotionPass error: %v", derr)
		}
		if demoted != 2 || freed != 420 {
			t.Errorf("Expected 2 demotions freeing 420B (1050->630<=800), got demoted=%d freed=%d", demoted, freed)
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
		if got := reg.GetTotalCacheSize(); got != 630 {
			t.Errorf("Expected TotalCacheSize=630 after demotion, got %d", got)
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
			StorageBackend:          "hybrid",
			CacheDir:                localDir,
			TmpDir:                  tmpDir,
			StateDir:                stateDir,
			CacheMaxBytes:           1000,
			HybridDemoteThreshold:   80,
			CacheRevalidateInterval: 1 * time.Hour,
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
			StorageBackend:          "hybrid",
			CacheDir:                localDir,
			TmpDir:                  tmpDir,
			StateDir:                stateDir,
			CacheMaxBytes:           100000,
			HybridDemoteThreshold:   80,
			HybridEagerS3Backup:     true,
			CacheRevalidateInterval: 1 * time.Hour,
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

func TestPhase3HybridInstantTierBothReclaimAndBoundedHeadroomNudge(t *testing.T) {
	setupHybridEnv := func(t *testing.T, bucket string, maxBytes int64, maxItemPct int, demoteThreshold int, preSeed func(tiered *TieredStorageBackend)) (*MockS3Server, *TieredStorageBackend, *CacheIndex, *HybridDemoter, string, string) {
		t.Helper()
		saveAndRestorePhase1Globals(t)
		clearPhase0ConfigEnv(t)

		mockS3 := NewMockS3Server(bucket)
		t.Cleanup(mockS3.Close)

		base := t.TempDir()
		cacheDir := filepath.Join(base, "cache")
		tmpDir := filepath.Join(base, "tmp")
		stateDir := filepath.Join(base, "state")
		for _, dir := range []string{cacheDir, tmpDir, stateDir} {
			if err := os.MkdirAll(dir, 0755); err != nil {
				t.Fatalf("MkdirAll %s: %v", dir, err)
			}
		}

		GlobalConfig = &Config{
			StorageBackend:          "hybrid",
			CacheDir:                cacheDir,
			TmpDir:                  tmpDir,
			StateDir:                stateDir,
			CacheMaxBytes:           maxBytes,
			CacheMaxItemPct:         maxItemPct,
			HybridDemoteThreshold:   demoteThreshold,
			CacheMinDiskFreeBytes:   0,
			TmpFileMaxBytes:         512 * 1024 * 1024,
			CacheRevalidateInterval: 1 * time.Hour,
			S3Config: S3Config{
				Endpoint:     mockS3.URL(),
				Bucket:       bucket,
				Region:       "us-east-1",
				AccessKey:    "test",
				SecretKey:    "test",
				UsePathStyle: true,
				DisableSSL:   true,
				ChunkSize:    64 * 1024,
			},
		}

		GlobalStorage = nil
		if err := InitStorage(); err != nil {
			t.Fatalf("InitStorage hybrid error: %v", err)
		}
		tiered := GlobalStorage.(*TieredStorageBackend)
		if preSeed != nil {
			preSeed(tiered)
		}
		InitRegistry()

		idx := getGlobalCacheIndex()
		demoter := getGlobalHybridDemoter()
		if idx == nil || demoter == nil {
			t.Fatalf("Expected initialized CacheIndex and HybridDemoter")
		}
		return mockS3, tiered, idx, demoter, cacheDir, tmpDir
	}

	seedTieredFiles := func(t *testing.T, tiered *TieredStorageBackend, key string, size int, withS3 bool, accessTime time.Time) {
		t.Helper()
		ctx := context.Background()
		p := tiered.Local().FilePath(key)
		payload := bytes.Repeat([]byte("A"), size)
		if err := os.WriteFile(p, payload, 0644); err != nil {
			t.Fatalf("WriteFile %s: %v", key, err)
		}
		meta := FileMeta{
			URL:           "http://origin/" + key,
			TotalSize:     int64(size),
			ContentType:   "video/mp4",
			LastValidated: accessTime.Unix(),
			Completed:     true,
		}
		metaBytes, _ := json.Marshal(meta)
		if err := os.WriteFile(p+".json", metaBytes, 0644); err != nil {
			t.Fatalf("WriteFile meta %s: %v", key, err)
		}
		_ = os.Chtimes(p, accessTime, accessTime)

		if withS3 {
			srcTmp := filepath.Join(t.TempDir(), key+".src")
			if err := os.WriteFile(srcTmp, payload, 0644); err != nil {
				t.Fatalf("WriteFile s3 src %s: %v", key, err)
			}
			if err := tiered.S3().CommitLocalFile(ctx, key, srcTmp, meta); err != nil {
				t.Fatalf("CommitLocalFile S3 %s: %v", key, err)
			}
			_ = os.Remove(srcTmp)
		}
	}

	cleanupState := func(s *FileCacheState) {
		if cm := s.GetChunkManager(); cm != nil {
			cm.Close()
		}
	}

	t.Run("InstantTierBothReclaim", func(t *testing.T) {
		now := time.Now()
		mockS3, tiered, idx, _, cacheDir, tmpDir := setupHybridEnv(t, "instant-reclaim-bucket", 4000, 50, 100, func(tb *TieredStorageBackend) {
			seedTieredFiles(t, tb, "backed_old.mp4", 2000, true, now.Add(-2*time.Hour))
			seedTieredFiles(t, tb, "disk_only.mp4", 1800, false, now.Add(-1*time.Hour))
		})

		if !waitForPhase1Condition(3*time.Second, func() bool {
			return idx.GetTier("backed_old.mp4") == IndexTierBoth && idx.GetTier("disk_only.mp4") == IndexTierDisk
		}) {
			t.Fatalf("Timed out waiting for startup reconcile (backed_old=%q, disk_only=%q)",
				idx.GetTier("backed_old.mp4"), idx.GetTier("disk_only.mp4"))
		}

		// Incoming 1500B item would exceed 4000B (3800 + 1500 = 5300), but backed_old.mp4 (2000B) is
		// already safely on S3 so AdmitToHybridL1 unlinks it synchronously and admits directly to TierDisk.
		s := NewFileCacheState("incoming_fit.mp4", "http://origin/incoming_fit.mp4",
			filepath.Join(cacheDir, "incoming_fit.mp4"), filepath.Join(tmpDir, "incoming_fit.part"), "", "video/mp4", "", "", 1500)
		defer cleanupState(s)

		if s.GetTargetTier() != TierDisk {
			t.Fatalf("Expected instant reclaim of Tier=both copy to admit 1500B to TierDisk, got %s", s.GetTargetTier())
		}
		if FileExists(tiered.Local().FilePath("backed_old.mp4")) {
			t.Errorf("Expected backed_old.mp4 local copy to be removed synchronously")
		}
		if !mockS3.HasObject("instant-reclaim-bucket", tiered.S3().monolithicKey("backed_old.mp4")) {
			t.Errorf("Expected backed_old.mp4 to remain in S3 after local reclaim")
		}
		if got := idx.GetTier("backed_old.mp4"); got != IndexTierS3 {
			t.Errorf("Expected backed_old.mp4 tier updated to %q, got %q", IndexTierS3, got)
		}
		if !FileExists(tiered.Local().FilePath("disk_only.mp4")) {
			t.Errorf("Expected unbacked disk_only.mp4 to remain untouched on Disk")
		}
		if stats := idx.LifecycleStats(); stats.DemotedDiskToS3 < 1 || stats.DemotedDiskToS3Bytes < 2000 {
			t.Errorf("Expected lifecycle demotion counter >= 1 (>=2000B), got count=%d bytes=%d",
				stats.DemotedDiskToS3, stats.DemotedDiskToS3Bytes)
		}
	})

	t.Run("BoundedReactiveHeadroomNudgeAt100Pct", func(t *testing.T) {
		now := time.Now()
		ctx := context.Background()
		mockS3, tiered, idx, demoter, cacheDir, tmpDir := setupHybridEnv(t, "headroom-nudge-bucket", 4000, 50, 100, func(tb *TieredStorageBackend) {
			seedTieredFiles(t, tb, "cold_a.mp4", 1900, false, now.Add(-2*time.Hour))
			seedTieredFiles(t, tb, "warm_b.mp4", 1900, false, now.Add(-10*time.Minute))
		})

		if !waitForPhase1Condition(3*time.Second, func() bool {
			return idx.GetTier("cold_a.mp4") == IndexTierDisk && idx.GetTier("warm_b.mp4") == IndexTierDisk
		}) {
			t.Fatalf("Timed out waiting for startup reconcile (cold_a=%q, warm_b=%q)",
				idx.GetTier("cold_a.mp4"), idx.GetTier("warm_b.mp4"))
		}
		idx.mu.Lock()
		if e := idx.entries["cold_a.mp4"]; e != nil {
			e.LastAccessed = now.Add(-2 * time.Hour).Unix()
		}
		if e := idx.entries["warm_b.mp4"]; e != nil {
			e.LastAccessed = now.Add(-10 * time.Minute).Unix()
		}
		idx.mu.Unlock()

		// 1. Oversized item (2500B > 2000B CACHE_MAX_ITEM_PCT limit) must bypass to S3 WITHOUT nudging Disk demotion.
		sOversize := NewFileCacheState("oversize.mp4", "http://origin/oversize.mp4",
			filepath.Join(cacheDir, "oversize.mp4"), filepath.Join(tmpDir, "oversize.part"), "", "video/mp4", "", "", 2500)
		defer cleanupState(sOversize)

		if sOversize.GetTargetTier() != TierS3 {
			t.Fatalf("Expected oversized 2500B item to route to TierS3, got %s", sOversize.GetTargetTier())
		}
		if demoted, _, err := demoter.RunDemotionPass(ctx); err != nil || demoted != 0 {
			t.Fatalf("Expected zero demotions after oversized bypass at 100%% threshold, got demoted=%d err=%v", demoted, err)
		}
		if !FileExists(tiered.Local().FilePath("cold_a.mp4")) || !FileExists(tiered.Local().FilePath("warm_b.mp4")) {
			t.Fatalf("Expected both local L1 assets untouched after oversized bypass")
		}

		// 2. Eligible item (1500B <= 2000B limit) cannot fit immediately (3800 + 1500 > 4000), so it routes cleanly
		//    to TierS3 AND nudges HybridDemoter for 1500B of headroom (target = 4000 - 1500 = 2500B).
		sEligible1 := NewFileCacheState("eligible_1.mp4", "http://origin/eligible_1.mp4",
			filepath.Join(cacheDir, "eligible_1.mp4"), filepath.Join(tmpDir, "eligible_1.part"), "", "video/mp4", "", "", 1500)
		defer cleanupState(sEligible1)

		if sEligible1.GetTargetTier() != TierS3 {
			t.Fatalf("Expected overflowing eligible 1500B item to bypass to TierS3 without blocking, got %s", sEligible1.GetTargetTier())
		}

		ok := waitForPhase1Condition(5*time.Second, func() bool {
			return !FileExists(tiered.Local().FilePath("cold_a.mp4")) && idx.GetTier("cold_a.mp4") == IndexTierS3
		})
		if !ok {
			t.Fatalf("Timed out waiting for nudged HybridDemoter to demote cold_a.mp4; exists=%v tier=%q",
				FileExists(tiered.Local().FilePath("cold_a.mp4")), idx.GetTier("cold_a.mp4"))
		}
		if !mockS3.HasObject("headroom-nudge-bucket", tiered.S3().monolithicKey("cold_a.mp4")) {
			t.Errorf("Expected cold_a.mp4 to be uploaded to S3")
		}
		// Bounded check: warm_b.mp4 (1900B <= target 2500B) must stay warm on Disk.
		if !FileExists(tiered.Local().FilePath("warm_b.mp4")) {
			t.Errorf("Expected warm_b.mp4 (1900B <= 2500B target) to stay warm on Disk")
		}

		// 3. Subsequent eligible 1500B item now fits directly on L1 Disk (1900 + 1500 = 3400 <= 4000).
		sEligible2 := NewFileCacheState("eligible_2.mp4", "http://origin/eligible_2.mp4",
			filepath.Join(cacheDir, "eligible_2.mp4"), filepath.Join(tmpDir, "eligible_2.part"), "", "video/mp4", "", "", 1500)
		defer cleanupState(sEligible2)

		if sEligible2.GetTargetTier() != TierDisk {
			t.Fatalf("Expected subsequent eligible 1500B item to admit to TierDisk after headroom demotion, got %s", sEligible2.GetTargetTier())
		}
	})

	t.Run("ReactiveNudgeCappedAt50PctWhenMaxItemPctDisabled", func(t *testing.T) {
		now := time.Now()
		mockS3, tiered, idx, _, cacheDir, tmpDir := setupHybridEnv(t, "nudge-cap-bucket", 4000, 0, 0, func(tb *TieredStorageBackend) {
			seedTieredFiles(t, tb, "f1.mp4", 900, false, now.Add(-4*time.Hour))
			seedTieredFiles(t, tb, "f2.mp4", 900, false, now.Add(-3*time.Hour))
			seedTieredFiles(t, tb, "f3.mp4", 900, false, now.Add(-2*time.Hour))
			seedTieredFiles(t, tb, "f4.mp4", 900, false, now.Add(-1*time.Hour))
		})

		if GlobalConfig.GetHybridDemoteThreshold() != 100 {
			t.Fatalf("Expected HybridDemoteThreshold to default to 100 when CACHE_MAX_ITEM_PCT=0, got %d",
				GlobalConfig.GetHybridDemoteThreshold())
		}
		if !waitForPhase1Condition(3*time.Second, func() bool {
			return idx.GetTier("f1.mp4") == IndexTierDisk &&
				idx.GetTier("f2.mp4") == IndexTierDisk &&
				idx.GetTier("f3.mp4") == IndexTierDisk &&
				idx.GetTier("f4.mp4") == IndexTierDisk
		}) {
			t.Fatalf("Timed out waiting for startup reconcile of f1..f4")
		}
		idx.mu.Lock()
		for i, k := range []string{"f1.mp4", "f2.mp4", "f3.mp4", "f4.mp4"} {
			if e := idx.entries[k]; e != nil {
				e.LastAccessed = now.Add(-time.Duration(4-i) * time.Hour).Unix()
			}
		}
		idx.mu.Unlock()

		// Incoming 3800B item (95% of 4000B) overflows L1 (3600 + 3800 > 4000), bypasses to S3,
		// and nudges HybridDemoter; cap of 50% (2000B) means at most 2000B target (demote f1+f2 -> 1800B left),
		// preserving f3 and f4 warm on Disk.
		sBig := NewFileCacheState("big_95pct.mp4", "http://origin/big_95pct.mp4",
			filepath.Join(cacheDir, "big_95pct.mp4"), filepath.Join(tmpDir, "big_95pct.part"), "", "video/mp4", "", "", 3800)
		defer cleanupState(sBig)

		if sBig.GetTargetTier() != TierS3 {
			t.Fatalf("Expected overflowing 3800B item to route to TierS3, got %s", sBig.GetTargetTier())
		}

		ok := waitForPhase1Condition(5*time.Second, func() bool {
			return !FileExists(tiered.Local().FilePath("f1.mp4")) &&
				!FileExists(tiered.Local().FilePath("f2.mp4")) &&
				idx.GetTier("f1.mp4") == IndexTierS3 &&
				idx.GetTier("f2.mp4") == IndexTierS3
		})
		if !ok {
			t.Fatalf("Timed out waiting for capped nudge to demote f1 and f2 (f1=%q, f2=%q)",
				idx.GetTier("f1.mp4"), idx.GetTier("f2.mp4"))
		}
		if !mockS3.HasObject("nudge-cap-bucket", tiered.S3().monolithicKey("f1.mp4")) ||
			!mockS3.HasObject("nudge-cap-bucket", tiered.S3().monolithicKey("f2.mp4")) {
			t.Errorf("Expected f1.mp4 and f2.mp4 uploaded to S3")
		}
		if !FileExists(tiered.Local().FilePath("f3.mp4")) || !FileExists(tiered.Local().FilePath("f4.mp4")) {
			t.Errorf("Expected f3.mp4 and f4.mp4 (total 1800B <= 2000B 50%% floor) to remain warm on Disk")
		}
	})
}
