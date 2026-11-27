package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func saveAndRestoreResilienceGlobals(t *testing.T) {
	t.Helper()
	oldCfg := GlobalConfig
	oldStorage := GlobalStorage
	oldReg := GlobalRegistry
	oldIdx := GlobalCacheIndex
	oldS3Idx := GlobalS3Index
	oldHealth := getGlobalStorageHealth()
	oldPM := GlobalPrehydrationManager
	oldBroker := GlobalLogBroker
	t.Cleanup(func() {
		if GlobalCacheIndex != nil && GlobalCacheIndex != oldIdx {
			GlobalCacheIndex.Stop()
		}
		if curHealth := getGlobalStorageHealth(); curHealth != nil && curHealth != oldHealth {
			curHealth.Stop()
		}
		GlobalConfig = oldCfg
		GlobalStorage = oldStorage
		GlobalRegistry = oldReg
		GlobalCacheIndex = oldIdx
		GlobalS3Index = oldS3Idx
		setGlobalStorageHealth(oldHealth)
		GlobalPrehydrationManager = oldPM
		GlobalLogBroker = oldBroker
	})
}

func TestPendingRecomposePreservesChunksServesChunkedHITAndTransitionsToMonolithic(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)

	const bucket = "pending-recompose-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	const chunkSize = int64(32 * 1024)
	const totalParts = 4
	totalSize := chunkSize * totalParts
	fullData := make([]byte, totalSize)
	_, _ = rand.Read(fullData)

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		CacheDir:                filepath.Join(tempDir, "cache"),
		TmpDir:                  filepath.Join(tempDir, "tmp"),
		StateDir:                filepath.Join(tempDir, "state"),
		TmpFileMaxBytes:         4 * 1024 * 1024,
		TmpMinDiskFreeBytes:     1024,
		CacheRevalidateInterval: 1 * time.Hour,
		StorageBackend:          "s3",
		S3Config: S3Config{
			Endpoint:             mockS3.URL(),
			Bucket:               bucket,
			Region:               "us-east-1",
			AccessKey:            "test-access",
			SecretKey:            "test-secret",
			UsePathStyle:         true,
			DisableSSL:           true,
			ChunkSize:            chunkSize,
			UploadConcurrency:    2,
			RecomposeConcurrency: 2,
		},
	}
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	_ = os.MkdirAll(GlobalConfig.StateDir, 0755)

	backend, err := NewS3StorageBackend(GlobalConfig.S3Config)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}
	GlobalStorage = backend
	GlobalRegistry = &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
	}
	setGlobalStorageHealth(nil)
	GlobalPrehydrationManager = nil
	GlobalLogBroker = nil

	idx := NewCacheIndex(filepath.Join(GlobalConfig.StateDir, "s3_index.json"))
	GlobalCacheIndex = idx
	GlobalS3Index = idx

	key := "pending_chunk_hit.mkv"
	ctx := context.Background()
	for i := 0; i < totalParts; i++ {
		off := int64(i) * chunkSize
		if err := backend.UploadChunk(ctx, key, i, bytes.NewReader(fullData[off:off+chunkSize]), chunkSize); err != nil {
			t.Fatalf("UploadChunk %d error: %v", i, err)
		}
	}

	// Age chunks > 24h to prove SweepStaleChunks exempts PendingRecompose entries.
	stale := time.Now().Add(-48 * time.Hour)
	for i := 0; i < totalParts; i++ {
		mockS3.SetObjectModTime(bucket, fmt.Sprintf("chunks/%s/chunk_%05d.bin", key, i), stale)
	}

	meta := FileMeta{
		URL:         "http://origin.example.com/" + key,
		TotalSize:   totalSize,
		ContentType: "video/x-matroska",
		Completed:   true,
	}
	idx.RecordPendingRecompose(key, meta, chunkSize, totalParts, true)

	// 1. StatsByTier must NOT count unsealed PendingRecompose entries as sealed S3 objects.
	_, _, s3Count, s3Bytes, _ := idx.StatsByTier()
	if s3Count != 0 || s3Bytes != 0 {
		t.Fatalf("Expected unsealed PendingRecompose not to count in StatsByTier, got count=%d bytes=%d", s3Count, s3Bytes)
	}

	// 2. Reconcile must preserve PendingRecompose even though vod/<key> does not exist yet.
	if err := idx.Reconcile(ctx, backend); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if !idx.IsPendingRecompose(key) {
		t.Fatalf("Expected Reconcile to preserve PendingRecompose entry for %s", key)
	}

	// 3. SweepStaleChunks must skip PendingRecompose chunks even when > 24h old.
	swept, err := backend.SweepStaleChunks(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleChunks error: %v", err)
	}
	if swept != 0 {
		t.Fatalf("Expected SweepStaleChunks to skip PendingRecompose chunks, swept=%d", swept)
	}

	// 4. StatWithTier must return a TierS3 HIT, and OpenWithMeta must return s3ChunkReadSeeker.
	statMeta, hitTier, exists, err := backend.StatWithTier(ctx, key)
	if err != nil || !exists || hitTier != TierS3 || statMeta == nil || statMeta.TotalSize != totalSize {
		t.Fatalf("Expected TierS3 chunked HIT from StatWithTier, got exists=%v tier=%q meta=%+v err=%v", exists, hitTier, statMeta, err)
	}

	reader, err := backend.OpenWithMeta(ctx, key, statMeta, hitTier)
	if err != nil {
		t.Fatalf("OpenWithMeta error: %v", err)
	}
	defer reader.Close()

	// Cross-chunk-boundary ReadAt spanning chunk 0 -> chunk 1 -> chunk 2.
	crossOff := chunkSize - 1024
	crossBuf := make([]byte, chunkSize+2048)
	n, err := reader.ReadAt(crossBuf, crossOff)
	if err != nil || n != len(crossBuf) {
		t.Fatalf("Cross-chunk ReadAt failed: n=%d want=%d err=%v", n, len(crossBuf), err)
	}
	if !bytes.Equal(crossBuf, fullData[crossOff:crossOff+int64(len(crossBuf))]) {
		t.Fatalf("Cross-chunk ReadAt payload mismatch")
	}

	// 5. Background retry seals vod/<key> and deletes chunks/<key>/ mid-stream;
	// the already-open s3ChunkReadSeeker must transparently fall back to vod/<key>.
	sealed := idx.ProcessPendingRecomposes(ctx, backend, GlobalRegistry, true)
	if sealed != 1 {
		t.Fatalf("Expected ProcessPendingRecomposes to seal 1 entry, got %d", sealed)
	}
	if idx.IsPendingRecompose(key) {
		t.Fatalf("Expected PendingRecompose to be cleared after seal")
	}
	if tier := idx.GetTier(key); tier != IndexTierS3 {
		t.Fatalf("Expected sealed entry tier='s3', got %q", tier)
	}
	if mockS3.HasObject(bucket, fmt.Sprintf("chunks/%s/chunk_00003.bin", key)) {
		t.Fatalf("Expected staging chunk 3 to be deleted after Recompose")
	}

	// Read chunk 3 from the already-open reader after chunks were deleted.
	tailBuf := make([]byte, chunkSize)
	nTail, err := reader.ReadAt(tailBuf, chunkSize*3)
	if err != nil || nTail != len(tailBuf) {
		t.Fatalf("Expected seamless fallback to monolithic vod/%s after chunks deleted, n=%d err=%v", key, nTail, err)
	}
	if !bytes.Equal(tailBuf, fullData[chunkSize*3:]) {
		t.Fatalf("Monolithic fallback ReadAt payload mismatch")
	}
}

func TestCommitCompletedFileFailedRecomposeKeepsActiveViewerPlaying(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)

	const bucket = "active-viewer-recompose-fail"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	const chunkSize = int64(32 * 1024)
	const totalParts = 3
	totalSize := chunkSize * totalParts
	fullData := make([]byte, totalSize)
	_, _ = rand.Read(fullData)

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		CacheDir:                filepath.Join(tempDir, "cache"),
		TmpDir:                  filepath.Join(tempDir, "tmp"),
		StateDir:                filepath.Join(tempDir, "state"),
		TmpFileMaxBytes:         4 * 1024 * 1024,
		TmpMinDiskFreeBytes:     1024,
		WritebackBufferSize:     64 * 1024,
		CacheRevalidateInterval: 1 * time.Hour,
		StorageBackend:          "s3",
		S3Config: S3Config{
			Endpoint:             mockS3.URL(),
			Bucket:               bucket,
			Region:               "us-east-1",
			AccessKey:            "test-access",
			SecretKey:            "test-secret",
			UsePathStyle:         true,
			DisableSSL:           true,
			ChunkSize:            chunkSize,
			UploadConcurrency:    2,
			RecomposeConcurrency: 2,
			RequestTimeout:       120 * time.Millisecond,
			RecomposeTimeout:     2 * time.Second,
		},
	}
	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	_ = os.MkdirAll(GlobalConfig.StateDir, 0755)

	backend, err := NewS3StorageBackend(GlobalConfig.S3Config)
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}
	GlobalStorage = backend
	InitRegistry()

	healthMon := NewStorageHealthMonitor(GlobalConfig, backend)
	setGlobalStorageHealth(healthMon)
	GlobalPrehydrationManager = nil
	GlobalLogBroker = nil

	idx := NewCacheIndex(filepath.Join(GlobalConfig.StateDir, "s3_index.json"))
	GlobalCacheIndex = idx
	GlobalS3Index = idx

	key := "active_viewer_recompose_fail.mkv"
	partPath := filepath.Join(GlobalConfig.TmpDir, key+".part")
	metaPath := filepath.Join(GlobalConfig.CacheDir, key+".json")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Truncate(totalSize)

	state := NewFileCacheState(key, "http://origin.example.com/"+key, filepath.Join(GlobalConfig.CacheDir, key), partPath, metaPath, "video/x-matroska", "", "", totalSize)
	state.File = f
	state.DiskMaterialized = true
	state.WriterActive = true
	state.UpstreamStatus = "Downloading"
	wp := NewWritebackPipeline(f, state, GlobalConfig.GetWritebackBufferSize())
	state.Pipeline = wp

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	reader := NewCacheReader(context.Background(), state, nil)

	for off := int64(0); off < totalSize; off += 16 * 1024 {
		end := off + 16*1024
		if err := wp.WriteChunk(off, fullData[off:end]); err != nil {
			t.Fatalf("WriteChunk error: %v", err)
		}
		state.RangeSet.Add(off, end)
	}

	// Ensure all chunks upload to S3 before UploadPartCopy stalls/fails.
	cm := state.GetChunkManager()
	if cm == nil {
		t.Fatalf("Expected ScratchChunkManager for TierS3")
	}
	if err := wp.FlushAndClose(); err != nil {
		t.Fatalf("FlushAndClose error: %v", err)
	}
	state.Lock.Lock()
	state.Pipeline = nil
	fileToClose := state.File
	state.File = nil
	state.Lock.Unlock()

	// Inject UploadPartCopy 500 failure BEFORE triggering chunk uploads so the automatic
	// AttemptRecompose on last chunk completion hits the injected error deterministically.
	mockS3.SetRequestHook(func(r *http.Request) int {
		if r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "" {
			return http.StatusInternalServerError
		}
		return 0
	})

	deadline := time.Now().Add(5 * time.Second)
	for !cm.AllChunksUploaded() && time.Now().Before(deadline) {
		cm.CheckAndTriggerUploads()
		time.Sleep(15 * time.Millisecond)
	}
	if !cm.AllChunksUploaded() {
		t.Fatalf("Timed out waiting for chunks to upload before Recompose failure injection")
	}

	state.Lock.Lock()
	state.IsCommitting = true
	state.UpstreamStatus = "Committing"
	state.Lock.Unlock()

	commitCompletedFile(state, fileToClose, nil, true, "[Test]")

	// Chunks MUST be preserved in S3, PendingRecompose recorded, and state kept alive for the active viewer.
	for i := 0; i < totalParts; i++ {
		if !mockS3.HasObject(bucket, fmt.Sprintf("chunks/%s/chunk_%05d.bin", key, i)) {
			t.Fatalf("Expected S3 chunk %d to remain intact after failed Recompose with active viewer", i)
		}
	}
	if !idx.IsPendingRecompose(key) {
		t.Fatalf("Expected PendingRecompose to be recorded in CacheIndex")
	}
	GlobalRegistry.Lock.RLock()
	_, stillRegistered := GlobalRegistry.Files[key]
	GlobalRegistry.Lock.RUnlock()
	if !stillRegistered {
		t.Fatalf("Expected %s to remain in GlobalRegistry.Files while active client is attached", key)
	}
	state.Lock.Lock()
	st := state.UpstreamStatus
	state.Lock.Unlock()
	if st != "Pending Recompose" {
		t.Fatalf("Expected UpstreamStatus='Pending Recompose', got %q", st)
	}

	// Active viewer must still read every byte cleanly from S3 chunks (not zeroes!).
	mockS3.SetRequestHook(nil)
	got := make([]byte, totalSize)
	for off := int64(0); off < totalSize; off += 16 * 1024 {
		n, rerr := reader.ReadAt(got[off:off+16*1024], off)
		if rerr != nil || n != 16*1024 {
			t.Fatalf("Active reader failed at offset %d after failed Recompose: n=%d err=%v", off, n, rerr)
		}
	}
	if !bytes.Equal(got, fullData) {
		t.Fatalf("Active reader received corrupted payload after failed Recompose")
	}

	// With S3 circuit open on last viewer exit, CacheReader.Close() releases local state
	// without deleting S3 chunks or blocking on another Recompose attempt.
	RecordS3Failure(errors.New("simulated s3 outage"))
	_ = reader.Close()

	GlobalRegistry.Lock.RLock()
	_, stillRegAfterClose := GlobalRegistry.Files[key]
	GlobalRegistry.Lock.RUnlock()
	if stillRegAfterClose {
		t.Fatalf("Expected %s to be removed from GlobalRegistry.Files after last viewer closed", key)
	}
	if !idx.IsPendingRecompose(key) {
		t.Fatalf("Expected PendingRecompose to remain in CacheIndex for background retry")
	}
}

func TestCancelHydrationRequiresForceWhenClientsActiveAndDisconnectsCleanly(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		CacheDir:        filepath.Join(tempDir, "cache"),
		TmpDir:          filepath.Join(tempDir, "tmp"),
		StateDir:        filepath.Join(tempDir, "state"),
		TmpFileMaxBytes: 4 * 1024 * 1024,
		StorageBackend:  "disk",
	}
	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	GlobalStorage = NewLocalStorageBackend(GlobalConfig.CacheDir, GlobalConfig.TmpDir)
	InitRegistry()
	GlobalPrehydrationManager = nil
	GlobalLogBroker = nil

	// 1. Active client attached: non-forced cancel must return 409 Conflict without cancelling.
	key := "cancel_guard_active.mkv"
	partPath := filepath.Join(GlobalConfig.TmpDir, key+".part")
	_ = os.WriteFile(partPath, make([]byte, 1024), 0644)

	coordCtx, coordCancel := context.WithCancel(context.Background())
	state := NewFileCacheState(key, "http://origin.example.com/"+key, filepath.Join(GlobalConfig.CacheDir, key), partPath, partPath+".json", "video/x-matroska", "", "", 1024)
	state.WriterActive = true
	state.Cancel = coordCancel

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	reader := NewCacheReader(context.Background(), state, nil)

	reqNoForce := httptest.NewRequest(http.MethodPost, "/api/hydration/cancel?key="+key, nil)
	rrNoForce := httptest.NewRecorder()
	handleCancelHydration(rrNoForce, reqNoForce)
	if rrNoForce.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict when cancelling active playback without force=true, got %d (%s)", rrNoForce.Code, rrNoForce.Body.String())
	}
	if coordCtx.Err() != nil {
		t.Fatalf("Expected coordinator NOT to be cancelled without force=true")
	}

	// 2. Forced cancel: returns 200 OK, cancels coordinator, unregisters active reader without panic.
	reqForce := httptest.NewRequest(http.MethodPost, "/api/hydration/cancel?key="+key+"&force=true", nil)
	rrForce := httptest.NewRecorder()
	handleCancelHydration(rrForce, reqForce)
	if rrForce.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK with force=true, got %d (%s)", rrForce.Code, rrForce.Body.String())
	}
	if coordCtx.Err() == nil {
		t.Fatalf("Expected coordinator context to be cancelled on forced cancel")
	}

	buf := make([]byte, 64)
	if _, err := reader.ReadAt(buf, 0); err == nil {
		t.Fatalf("Expected active reader ReadAt to fail cleanly after forced CancelCoordinator")
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Expected reader.Close() not to error/panic after forced CancelCoordinator, got: %v", err)
	}

	// 3. Prehydration (0 active clients): non-forced cancel proceeds with 200 OK.
	preKey := "cancel_prehydration_idle.mkv"
	prePart := filepath.Join(GlobalConfig.TmpDir, preKey+".part")
	_ = os.WriteFile(prePart, make([]byte, 512), 0644)
	preCtx, preCancel := context.WithCancel(context.Background())
	preState := NewFileCacheState(preKey, "http://origin.example.com/"+preKey, filepath.Join(GlobalConfig.CacheDir, preKey), prePart, prePart+".json", "video/x-matroska", "", "", 512)
	preState.WriterActive = true
	preState.IsPrehydrating = true
	preState.Cancel = preCancel

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[preKey] = preState
	GlobalRegistry.Lock.Unlock()

	reqPre := httptest.NewRequest(http.MethodPost, "/api/hydration/cancel?key="+preKey, nil)
	rrPre := httptest.NewRecorder()
	handleCancelHydration(rrPre, reqPre)
	if rrPre.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for idle prehydration cancel without force=true, got %d", rrPre.Code)
	}
	if preCtx.Err() == nil {
		t.Fatalf("Expected idle prehydration context to be cancelled")
	}
}

func TestManualAddWorkerSplitsLargestGapAndRespectsGuards(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)

	// 1. Unit check: findLargestGapMidpoint splits around active worker cursors.
	gapState := &FileCacheState{
		TotalSize: 100 * 1024 * 1024,
		RangeSet:  NewRangeSet(),
	}
	gapState.RangeSet.Add(0, 10*1024*1024) // gap is [10MB, 100MB)
	gapState.ActiveWorkers = []*Worker{
		{id: 1, currentOffset: 10 * 1024 * 1024},
		{id: 2, currentOffset: 70 * 1024 * 1024},
	}
	mid, gs, ge, ok := findLargestGapMidpoint(gapState)
	if !ok || gs != 10*1024*1024 || ge != 70*1024*1024 || mid != 40*1024*1024 {
		t.Fatalf("Expected largest unclaimed span [10MB, 70MB) midpoint 40MB, got ok=%v [%d, %d) mid=%d", ok, gs, ge, mid)
	}

	// 2. End-to-end API + Coordinator check on a throttled prehydration.
	const totalSize = 256 * 1024 // 256 KB
	payload := make([]byte, totalSize)
	_, _ = rand.Read(payload)

	var mu sync.Mutex
	var requestedStarts []int64
	gateWorker1 := make(chan struct{})
	var gateOnce sync.Once

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(totalSize))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(payload)
			}
			return
		}
		trimmed := strings.TrimPrefix(rng, "bytes=")
		parts := strings.SplitN(trimmed, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		end := int64(totalSize - 1)
		if parts[1] != "" {
			if parsedEnd, err := strconv.ParseInt(parts[1], 10, 64); err == nil && parsedEnd < end {
				end = parsedEnd
			}
		}

		// Ignore 1-byte probe requests when tracking worker ranges.
		isSingleByteProbe := start == 0 && end == 0
		if !isSingleByteProbe {
			mu.Lock()
			requestedStarts = append(requestedStarts, start)
			mu.Unlock()
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, totalSize))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)

		// Throttle initial worker starting at offset 0 so we can click +Worker mid-download.
		if start == 0 && !isSingleByteProbe {
			_, _ = w.Write(payload[:32*1024])
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			select {
			case <-gateWorker1:
			case <-r.Context().Done():
				return
			}
			// Wait slightly for targetEndOffset clamping to propagate when Worker 2 spawns.
			time.Sleep(50 * time.Millisecond)
			for off := int64(32 * 1024); off <= end; off += 16 * 1024 {
				chunkEnd := off + 16*1024
				if chunkEnd > end+1 {
					chunkEnd = end + 1
				}
				if _, err := w.Write(payload[off:chunkEnd]); err != nil {
					return
				}
				if fl, ok := w.(http.Flusher); ok {
					fl.Flush()
				}
				select {
				case <-r.Context().Done():
					return
				default:
				}
			}
			return
		}

		_, _ = w.Write(payload[start : end+1])
	}))
	defer origin.Close()

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		CacheDir:                filepath.Join(tempDir, "cache"),
		TmpDir:                  filepath.Join(tempDir, "tmp"),
		StateDir:                filepath.Join(tempDir, "state"),
		TmpFileMaxBytes:         4 * 1024 * 1024,
		CacheMaxBytes:           16 * 1024 * 1024,
		CacheMaxItemPct:         90,
		MaxUpstreamWorkers:      1, // Initially 1 -> API must reject
		SchedulerMinTime:        100 * time.Millisecond,
		ClientHydrationWait:     500 * time.Millisecond,
		StarvationStallTimeout:  10 * time.Second,
		WritebackBufferSize:     64 * 1024,
		CacheRevalidateInterval: 1 * time.Hour,
		StorageBackend:          "disk",
	}
	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	InitHTTPClient()
	GlobalStorage = NewLocalStorageBackend(GlobalConfig.CacheDir, GlobalConfig.TmpDir)
	InitRegistry()
	GlobalPrehydrationManager = nil
	GlobalLogBroker = nil

	key := "manual_add_worker.mkv"
	partPath := filepath.Join(GlobalConfig.TmpDir, key+".part")
	finalPath := filepath.Join(GlobalConfig.CacheDir, key)
	state := NewFileCacheState(key, origin.URL+"/video.mkv", finalPath, partPath, finalPath+".json", "video/x-matroska", "", "", totalSize)
	state.IsPrehydrating = true

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	defer func() {
		gateOnce.Do(func() { close(gateWorker1) })
		CancelCoordinator(state)
	}()

	StartCoordinator(state, nil)

	// Wait until Worker 1 has downloaded the initial 32 KB.
	waitDL := time.Now().Add(3 * time.Second)
	for state.RangeSet.TotalBytes() < 32*1024 && time.Now().Before(waitDL) {
		time.Sleep(15 * time.Millisecond)
	}
	if state.RangeSet.TotalBytes() < 32*1024 {
		t.Fatalf("Timed out waiting for initial worker download")
	}

	// Guard A: MaxUpstreamWorkers == 1 -> must reject 409 Conflict.
	req1 := httptest.NewRequest(http.MethodPost, "/api/hydration/add-worker?key="+key, nil)
	rr1 := httptest.NewRecorder()
	handleAddWorker(rr1, req1)
	if rr1.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict when MaxUpstreamWorkers=1, got %d", rr1.Code)
	}

	// Raise MaxUpstreamWorkers to 2.
	GlobalConfig.Lock()
	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.Unlock()

	// Guard B: Uncovered player skip (Starving) active -> must reject 409 Conflict.
	state.Lock.Lock()
	state.Starving["skip-client"] = 192 * 1024
	state.Lock.Unlock()
	reqSkip := httptest.NewRequest(http.MethodPost, "/api/hydration/add-worker?key="+key, nil)
	rrSkip := httptest.NewRecorder()
	handleAddWorker(rrSkip, reqSkip)
	if rrSkip.Code != http.StatusConflict {
		t.Fatalf("Expected 409 Conflict while player skip is uncovered, got %d", rrSkip.Code)
	}
	state.Lock.Lock()
	delete(state.Starving, "skip-client")
	state.Lock.Unlock()

	// Valid manual +Worker on prehydration (1/2 active): spawns Worker 2 at midpoint of remaining gap.
	reqAdd := httptest.NewRequest(http.MethodPost, "/api/hydration/add-worker?key="+key, nil)
	rrAdd := httptest.NewRecorder()
	handleAddWorker(rrAdd, reqAdd)
	if rrAdd.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from handleAddWorker, got %d (%s)", rrAdd.Code, rrAdd.Body.String())
	}

	// Wait for Worker 2 request to hit origin at midpoint > 64KB (not 0).
	waitW2 := time.Now().Add(3 * time.Second)
	var worker2Start int64 = -1
	for time.Now().Before(waitW2) {
		mu.Lock()
		for _, s := range requestedStarts {
			if s > 32*1024 {
				worker2Start = s
				break
			}
		}
		mu.Unlock()
		if worker2Start > 0 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if worker2Start <= 32*1024 || worker2Start >= totalSize {
		t.Fatalf("Expected manual Worker 2 to start near midpoint of remaining gap (>32KB, <%d), got %d (starts=%v)", totalSize, worker2Start, requestedStarts)
	}

	// Release Worker 1 and wait for full file commit.
	gateOnce.Do(func() { close(gateWorker1) })
	waitDone := time.Now().Add(5 * time.Second)
	for !FileExists(finalPath) && time.Now().Before(waitDone) {
		time.Sleep(20 * time.Millisecond)
	}
	if !FileExists(finalPath) {
		t.Fatalf("Timed out waiting for dual-worker hydration to commit %s", finalPath)
	}
	gotBytes, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if !bytes.Equal(gotBytes, payload) {
		t.Fatalf("Committed payload mismatch after manual +Worker (%d vs %d bytes)", len(gotBytes), len(payload))
	}
}
