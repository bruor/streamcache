package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestS3Backend(t *testing.T, mock *MockS3Server, bucket string) *S3StorageBackend {
	t.Helper()
	b, err := NewS3StorageBackend(S3Config{
		Endpoint:     mock.URL(),
		Bucket:       bucket,
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    64 * 1024,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}
	return b
}

func TestS3ConfigDefaults(t *testing.T) {
	t.Setenv("S3_MAX_IDLE_AGE", "")
	_ = os.Unsetenv("S3_MAX_IDLE_AGE")
	t.Setenv("S3_EVICT_INTERVAL", "")
	_ = os.Unsetenv("S3_EVICT_INTERVAL")

	cfg := parseConfigFromEnv()
	if cfg.GetS3MaxIdleAge() != 720*time.Hour {
		t.Fatalf("Expected default S3MaxIdleAge=720h, got %s", cfg.GetS3MaxIdleAge())
	}
	if cfg.GetS3EvictInterval() != 6*time.Hour {
		t.Fatalf("Expected default S3EvictInterval=6h, got %s", cfg.GetS3EvictInterval())
	}
}

func TestS3StorageBackend_List(t *testing.T) {
	const bucket = "list-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()
	backend := newTestS3Backend(t, mockS3, bucket)

	tempDir, err := os.MkdirTemp("", "sc2_s3_list_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	for _, k := range []string{"ep1.mkv", "ep2.mkv"} {
		partPath := filepath.Join(tempDir, k+".part")
		data := []byte("data-" + k)
		_ = os.WriteFile(partPath, data, 0644)
		if err := backend.CommitLocalFile(ctx, k, partPath, FileMeta{TotalSize: int64(len(data))}); err != nil {
			t.Fatalf("CommitLocalFile %s error: %v", k, err)
		}
	}

	// Upload a temporary chunk under chunks/ and verify List only returns vod/ objects
	if err := backend.UploadChunk(ctx, "ep3.mkv", 0, bytes.NewReader([]byte("chunkdata")), 9); err != nil {
		t.Fatalf("UploadChunk error: %v", err)
	}

	objs, err := backend.List(ctx)
	if err != nil {
		t.Fatalf("List error: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("Expected 2 vod objects in List, got %d (%+v)", len(objs), objs)
	}
	if objs[0].Key != "ep1.mkv" || objs[1].Key != "ep2.mkv" {
		t.Fatalf("Unexpected listed keys: %+v", objs)
	}
	if objs[0].Size != int64(len("data-ep1.mkv")) {
		t.Errorf("Unexpected size for ep1.mkv: %d", objs[0].Size)
	}
}

func TestS3AccessIndex_PersistenceAndReconcile(t *testing.T) {
	const bucket = "reconcile-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()
	backend := newTestS3Backend(t, mockS3, bucket)

	tempDir, err := os.MkdirTemp("", "sc2_s3_idx_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	for _, k := range []string{"ep1.mkv", "ep2.mkv"} {
		partPath := filepath.Join(tempDir, k+".part")
		data := []byte("payload-" + k)
		_ = os.WriteFile(partPath, data, 0644)
		if err := backend.CommitLocalFile(ctx, k, partPath, FileMeta{TotalSize: int64(len(data))}); err != nil {
			t.Fatalf("CommitLocalFile %s error: %v", k, err)
		}
	}

	preExistingMod := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mockS3.SetObjectModTime(bucket, "vod/ep2.mkv", preExistingMod)

	indexPath := filepath.Join(tempDir, "s3_index.json")
	idx := NewS3AccessIndex(indexPath)
	t1 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	idx.nowFn = func() time.Time { return t1 }

	// Record ep1 and a stale deleted_ep locally, flush, then reload in a fresh instance
	idx.RecordCommit("ep1.mkv", "http://origin/ep1.mkv", 15)
	idx.RecordCommit("stale_deleted.mkv", "http://origin/stale.mkv", 100)

	reloaded := NewS3AccessIndex(indexPath)
	reloaded.nowFn = func() time.Time { return t1 }
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load error: %v", err)
	}

	if err := reloaded.Reconcile(ctx, backend); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	count, _ := reloaded.Stats()
	if count != 2 {
		t.Fatalf("Expected 2 tracked objects after reconcile (added ep2, pruned stale_deleted), got %d", count)
	}

	reloaded.mu.Lock()
	ep1Entry := reloaded.entries["ep1.mkv"]
	ep2Entry := reloaded.entries["ep2.mkv"]
	staleEntry := reloaded.entries["stale_deleted.mkv"]
	reloaded.mu.Unlock()

	if staleEntry != nil {
		t.Errorf("Expected stale_deleted.mkv to be pruned on reconcile")
	}
	if ep1Entry == nil || ep1Entry.LastAccessed != t1.Unix() {
		t.Errorf("Expected ep1.mkv LastAccessed=%d preserved, got %+v", t1.Unix(), ep1Entry)
	}
	if ep2Entry == nil || ep2Entry.LastAccessed != preExistingMod.Unix() {
		t.Errorf("Expected ep2.mkv LastAccessed seeded from S3 LastModified (%d), got %+v", preExistingMod.Unix(), ep2Entry)
	}
}

func TestS3AccessIndex_IdleEvictionAndActiveGuards(t *testing.T) {
	const bucket = "evict-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()
	backend := newTestS3Backend(t, mockS3, bucket)

	tempDir, err := os.MkdirTemp("", "sc2_s3_evict_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	ctx := context.Background()
	keys := []string{"old_idle.mkv", "fresh.mkv", "old_active_conn.mkv", "old_active_file.mkv"}
	for _, k := range keys {
		partPath := filepath.Join(tempDir, k+".part")
		data := []byte("content-" + k)
		_ = os.WriteFile(partPath, data, 0644)
		if err := backend.CommitLocalFile(ctx, k, partPath, FileMeta{TotalSize: int64(len(data))}); err != nil {
			t.Fatalf("CommitLocalFile %s error: %v", k, err)
		}
	}

	baseNow := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	curNow := baseNow
	idx := NewS3AccessIndex(filepath.Join(tempDir, "s3_index.json"))
	idx.nowFn = func() time.Time { return curNow }

	for _, k := range keys {
		idx.RecordCommit(k, "http://origin/"+k, int64(len("content-"+k)))
	}

	// Advance clock by 20 days and touch only fresh.mkv
	curNow = baseNow.Add(20 * 24 * time.Hour)
	idx.RecordAccess("fresh.mkv", "", 0)

	// Advance clock to 31 days after baseNow (fresh.mkv is only 11d idle; others are 31d idle)
	curNow = baseNow.Add(31 * 24 * time.Hour)

	reg := &CacheRegistry{
		Files: map[string]*FileCacheState{
			"old_active_file.mkv": {Key: "old_active_file.mkv"},
		},
		Connections: map[string]*ClientConnection{
			"conn-1": {ID: "conn-1", Key: "old_active_conn.mkv"},
		},
	}

	// First check disabled maxIdleAge <= 0 does nothing
	evDisabled, _ := idx.SweepExpired(ctx, backend, reg, 0)
	if evDisabled != 0 {
		t.Fatalf("Expected 0 evicted when maxIdleAge=0, got %d", evDisabled)
	}

	// Now sweep with 720h (30d)
	evicted, freed := idx.SweepExpired(ctx, backend, reg, 720*time.Hour)
	if evicted != 1 {
		t.Fatalf("Expected exactly 1 evicted (old_idle.mkv), got %d", evicted)
	}
	if freed != int64(len("content-old_idle.mkv")) {
		t.Errorf("Unexpected freed bytes: %d", freed)
	}

	if mockS3.HasObject(bucket, "vod/old_idle.mkv") {
		t.Errorf("Expected old_idle.mkv to be deleted from S3")
	}
	for _, kept := range []string{"fresh.mkv", "old_active_conn.mkv", "old_active_file.mkv"} {
		if !mockS3.HasObject(bucket, "vod/"+kept) {
			t.Errorf("Expected %s to remain in S3", kept)
		}
	}
}

func TestS3AccessIndex_ProbeIgnoredVsPlaybackAndChainTouch(t *testing.T) {
	const bucket = "chain-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()
	backend := newTestS3Backend(t, mockS3, bucket)

	tempDir, err := os.MkdirTemp("", "sc2_s3_chain_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Save & restore globals cleanly
	origConfig := GlobalConfig
	origStorage := GlobalStorage
	origRegistry := GlobalRegistry
	origS3Idx := GlobalS3Index
	origPM := GlobalPrehydrationManager
	defer func() {
		if GlobalS3Index != nil {
			GlobalS3Index.Stop()
		}
		GlobalConfig = origConfig
		GlobalStorage = origStorage
		GlobalRegistry = origRegistry
		GlobalS3Index = origS3Idx
		GlobalPrehydrationManager = origPM
	}()

	GlobalConfig = &Config{
		CacheDir:                  tempDir,
		TmpDir:                    filepath.Join(tempDir, "tmp"),
		CacheRevalidateInterval:   24 * time.Hour,
		MinReadThresholdBytes:     64 * 1024, // 64 KB threshold for test speed
		EnableEpisodePrehydration: true,
		StorageBackend:            "s3",
		S3MaxIdleAge:              720 * time.Hour,
		S3EvictInterval:           6 * time.Hour,
	}
	GlobalStorage = backend
	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
	}

	// Prehydration manager without background scheduler loop so nothing tries to hit upstream URLs
	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}

	ctx := context.Background()
	ep1URL := "http://origin.example.com/ep1.mkv"
	ep2URL := "http://origin.example.com/ep2.mkv"
	ep3URL := "http://origin.example.com/ep3.mkv"

	cacheFileKey := func(u string) string {
		k, ext := getCacheKeyAndExt(u)
		return k + ext
	}
	ep1Key := cacheFileKey(ep1URL)
	ep2Key := cacheFileKey(ep2URL)
	ep3Key := cacheFileKey(ep3URL)

	payload := bytes.Repeat([]byte("A"), 128*1024) // 128 KB > 64 KB MinReadThresholdBytes
	for _, k := range []string{ep1Key, ep2Key, ep3Key} {
		partPath := filepath.Join(tempDir, k+".part")
		_ = os.WriteFile(partPath, payload, 0644)
		if err := backend.CommitLocalFile(ctx, k, partPath, FileMeta{
			URL:           "http://origin.example.com/" + k,
			TotalSize:     int64(len(payload)),
			ContentType:   "video/x-matroska",
			LastValidated: time.Now().Unix(),
			Completed:     true,
		}); err != nil {
			t.Fatalf("CommitLocalFile error: %v", err)
		}
	}

	baseTime := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	curTime := baseTime
	idx := NewS3AccessIndex(filepath.Join(tempDir, "s3_index.json"))
	idx.nowFn = func() time.Time { return curTime }
	idx.RecordCommit(ep1Key, ep1URL, int64(len(payload)))
	idx.RecordCommit(ep2Key, ep2URL, int64(len(payload)))
	idx.RecordCommit(ep3Key, ep3URL, int64(len(payload)))
	GlobalS3Index = idx

	// Advance clock by 20 days
	curTime = baseTime.Add(20 * 24 * time.Hour)

	buildStreamURL := func(u string, next []string) string {
		q := url.Values{}
		q.Set("url", u)
		for _, n := range next {
			q.Add("next_url", n)
		}
		return "/stream?" + q.Encode()
	}

	// 1. HEAD probe MUST NOT refresh ep1, ep2, or ep3
	headReq := httptest.NewRequest(http.MethodHead, buildStreamURL(ep1URL, []string{ep2URL, ep3URL}), nil)
	headRec := httptest.NewRecorder()
	handleMediaStream(headRec, headReq)
	if headRec.Code != http.StatusOK {
		t.Fatalf("Expected 200 on HEAD, got %d", headRec.Code)
	}

	// 2. Small GET range probe (4 KB < 64 KB) MUST NOT refresh ep1, ep2, or ep3
	probeReq := httptest.NewRequest(http.MethodGet, buildStreamURL(ep1URL, []string{ep2URL, ep3URL}), nil)
	probeReq.Header.Set("Range", "bytes=0-4095")
	probeRec := httptest.NewRecorder()
	handleMediaStream(probeRec, probeReq)
	if probeRec.Code != http.StatusPartialContent {
		t.Fatalf("Expected 206 on small probe range, got %d", probeRec.Code)
	}

	idx.mu.Lock()
	for _, k := range []string{ep1Key, ep2Key, ep3Key} {
		if idx.entries[k].LastAccessed != baseTime.Unix() {
			t.Errorf("Probe unexpectedly updated LastAccessed for %s: got %d, want %d", k, idx.entries[k].LastAccessed, baseTime.Unix())
		}
	}
	idx.mu.Unlock()

	// 3. Real GET playback (full 128 KB >= 64 KB threshold) MUST refresh ep1 AND chained ep2 + ep3
	playReq := httptest.NewRequest(http.MethodGet, buildStreamURL(ep1URL, []string{ep2URL, ep3URL}), nil)
	playRec := httptest.NewRecorder()
	handleMediaStream(playRec, playReq)
	if playRec.Code != http.StatusOK || playRec.Body.Len() != len(payload) {
		t.Fatalf("Expected full 200 playback (%d bytes), got status=%d len=%d", len(payload), playRec.Code, playRec.Body.Len())
	}

	idx.mu.Lock()
	for _, k := range []string{ep1Key, ep2Key, ep3Key} {
		if idx.entries[k].LastAccessed != curTime.Unix() {
			t.Errorf("Real playback failed to update LastAccessed for %s: got %d, want %d", k, idx.entries[k].LastAccessed, curTime.Unix())
		}
	}
	idx.mu.Unlock()
}

func TestS3PrehydrationChainsBeforeRecomposeCompletes(t *testing.T) {
	const bucket = "early-chain-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	tempDir, err := os.MkdirTemp("", "sc2_s3_early_chain_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	oldConfig := GlobalConfig
	oldStorage := GlobalStorage
	oldRegistry := GlobalRegistry
	oldPM := GlobalPrehydrationManager
	defer func() {
		if GlobalS3Index != nil {
			GlobalS3Index.Stop()
		}
		GlobalConfig = oldConfig
		GlobalStorage = oldStorage
		GlobalRegistry = oldRegistry
		GlobalPrehydrationManager = oldPM
	}()

	GlobalConfig = &Config{
		CacheDir:                filepath.Join(tempDir, "files"),
		TmpDir:                  filepath.Join(tempDir, "tmp"),
		CacheRevalidateInterval: 1 * time.Hour,
		WritebackBufferSize:     64 * 1024,
		StorageBackend:          "s3",
		S3Config: S3Config{
			ChunkSize:           32 * 1024,
			ScratchMaxBytes:     10 * 1024 * 1024,
			ScratchMinFreeBytes: 1024,
		},
	}
	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)

	rawBackend := newTestS3Backend(t, mockS3, bucket)
	rawBackend.chunkSize = 32 * 1024
	delayed := &delayedRecomposeStorage{
		S3StorageBackend: rawBackend,
		recomposeStarted: make(chan struct{}),
		releaseRecompose: make(chan struct{}),
	}
	GlobalStorage = delayed

	InitRegistry()

	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}

	key := "early_chain_ep1.mkv"
	totalSize := int64(96 * 1024)
	fullData := bytes.Repeat([]byte("Z"), int(totalSize))
	partPath := filepath.Join(GlobalConfig.TmpDir, key+".part")
	metaPath := filepath.Join(GlobalConfig.CacheDir, key+".json")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("OpenFile error: %v", err)
	}
	_ = f.Truncate(totalSize)

	state := NewFileCacheState(key, "http://origin.example.com/ep1.mkv", filepath.Join(GlobalConfig.CacheDir, key), partPath, metaPath, "video/x-matroska", "", "", totalSize)
	state.File = f
	state.DiskMaterialized = true
	state.WriterActive = true
	state.IsPrehydrating = true
	state.IsCommitting = true
	state.UpstreamStatus = "Committing"
	state.NextURLs = []string{"http://origin.example.com/ep2.mkv", "http://origin.example.com/ep3.mkv"}
	wp := NewWritebackPipeline(f, state, GlobalConfig.GetWritebackBufferSize())
	state.Pipeline = wp

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	GlobalPrehydrationManager.mu.Lock()
	GlobalPrehydrationManager.activePrehydrations[key] = true
	GlobalPrehydrationManager.prehydrationsStartedSinceLastPoll = 1
	GlobalPrehydrationManager.mu.Unlock()

	for off := int64(0); off < totalSize; off += 32 * 1024 {
		end := off + 32*1024
		if err := wp.WriteChunk(off, fullData[off:end]); err != nil {
			t.Fatalf("WriteChunk error: %v", err)
		}
		state.RangeSet.Add(off, end)
	}

	state.Lock.Lock()
	fileToClose := state.File
	state.File = nil
	pipelineToClose := state.Pipeline
	state.Pipeline = nil
	state.Lock.Unlock()

	commitDone := make(chan struct{})
	go func() {
		finalizeAndCommitCache(state, fileToClose, pipelineToClose)
		close(commitDone)
	}()

	select {
	case <-delayed.recomposeStarted:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for Recompose to start")
	}

	// While S3 Recompose is still blocked, next episode MUST already be queued and ep1 cleared from activePrehydrations
	GlobalPrehydrationManager.mu.Lock()
	stillActive := GlobalPrehydrationManager.activePrehydrations[key]
	qLen := len(GlobalPrehydrationManager.queue)
	var queuedURL string
	if qLen > 0 {
		queuedURL = GlobalPrehydrationManager.queue[0].TargetURL
	}
	GlobalPrehydrationManager.mu.Unlock()

	if stillActive {
		t.Errorf("Expected %s to be cleared from activePrehydrations before Recompose finishes", key)
	}
	if qLen != 1 || queuedURL != "http://origin.example.com/ep2.mkv" {
		t.Fatalf("Expected ep2.mkv to be queued while Recompose is running, got qLen=%d url=%q", qLen, queuedURL)
	}

	close(delayed.releaseRecompose)
	select {
	case <-commitDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for finalizeAndCommitCache to complete")
	}
}

func TestLoadExistingCachePreservesS3Index(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "sc2_load_s3_index_*")
	if err != nil {
		t.Fatalf("MkdirTemp error: %v", err)
	}
	defer os.RemoveAll(tempDir)

	origConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir: filepath.Join(tempDir, "files"),
		TmpDir:   filepath.Join(tempDir, "tmp"),
	}
	defer func() { GlobalConfig = origConfig }()

	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)

	// Write s3_index.json, a crash temp file, and an orphaned media .json
	s3IndexPath := filepath.Join(GlobalConfig.CacheDir, "s3_index.json")
	tmpIndexPath := filepath.Join(GlobalConfig.CacheDir, "s3_index.json.tmp.12345")
	orphanMetaPath := filepath.Join(GlobalConfig.CacheDir, "orphan_movie.mkv.json")
	_ = os.WriteFile(s3IndexPath, []byte(`{"ep1.mkv":{"key":"ep1.mkv","last_accessed":1700000000}}`), 0644)
	_ = os.WriteFile(tmpIndexPath, []byte(`partial`), 0644)
	_ = os.WriteFile(orphanMetaPath, []byte(`{}`), 0644)

	reg := &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
		Sessions:    make(map[string]*ClientSession),
	}
	reg.LoadExistingCache()

	if !FileExists(s3IndexPath) {
		t.Fatalf("Expected LoadExistingCache to preserve s3_index.json")
	}
	if FileExists(tmpIndexPath) {
		t.Errorf("Expected LoadExistingCache to clean up s3_index.json.tmp.*")
	}
	if FileExists(orphanMetaPath) {
		t.Errorf("Expected LoadExistingCache to clean up orphan_movie.mkv.json")
	}
}

func TestClientSessionCacheSourceS3(t *testing.T) {
	origConfig := GlobalConfig
	origRegistry := GlobalRegistry
	defer func() {
		GlobalConfig = origConfig
		GlobalRegistry = origRegistry
	}()

	GlobalConfig = &Config{StorageBackend: "s3"}
	reg := &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
		Sessions:    make(map[string]*ClientSession),
	}
	GlobalRegistry = reg

	reg.RegisterConnection("conn-s3-1", &ClientConnection{
		ID:          "conn-s3-1",
		Key:         "ep1.mkv",
		URL:         "http://origin/ep1.mkv",
		RemoteAddr:  "127.0.0.1:1234",
		UserAgent:   "Emby",
		TotalSize:   1000,
		StartTime:   time.Now().Unix(),
		InCache:     true,
		CacheSource: "s3",
	})
	defer reg.UnregisterConnection("conn-s3-1")

	sessions := reg.GetActiveSessions()
	if len(sessions) != 1 || sessions[0].CacheSource != "s3" {
		t.Fatalf("Expected active session CacheSource='s3', got %+v", sessions)
	}

	raw, err := buildStatePayload()
	if err != nil {
		t.Fatalf("buildStatePayload error: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("Unmarshal buildStateJSON error: %v", err)
	}
	if payload["storage_backend"] != "s3" {
		t.Errorf("Expected storage_backend='s3' in SSE state, got %v", payload["storage_backend"])
	}
}
