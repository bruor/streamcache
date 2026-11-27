package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	oldDemoter := getGlobalHybridDemoter()
	oldHealth := getGlobalStorageHealth()
	oldPM := GlobalPrehydrationManager
	oldBroker := GlobalLogBroker
	t.Cleanup(func() {
		if d := getGlobalHybridDemoter(); d != nil && d != oldDemoter {
			d.Stop()
		}
		if GlobalCacheIndex != nil && GlobalCacheIndex != oldIdx {
			GlobalCacheIndex.Stop()
		}
		if curHealth := getGlobalStorageHealth(); curHealth != nil && curHealth != oldHealth {
			curHealth.Stop()
		}
		if GlobalRegistry != nil && GlobalRegistry != oldReg {
			GlobalRegistry.Stop()
		}
		GlobalConfig = oldCfg
		GlobalStorage = oldStorage
		GlobalRegistry = oldReg
		GlobalCacheIndex = oldIdx
		GlobalS3Index = oldS3Idx
		setGlobalHybridDemoter(oldDemoter)
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

	// Release Worker 1 and wait for full file commit and registry cleanup.
	gateOnce.Do(func() { close(gateWorker1) })
	waitDone := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDone) {
		GlobalRegistry.Lock.RLock()
		_, stillActive := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.RUnlock()
		if !stillActive && FileExists(finalPath) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(25 * time.Millisecond)
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

func TestDashboardHeadersAndPendingRecomposeVisibility(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		CacheDir:       filepath.Join(tempDir, "cache"),
		TmpDir:         filepath.Join(tempDir, "tmp"),
		StateDir:       filepath.Join(tempDir, "state"),
		StorageBackend: "disk",
	}
	InitRegistry()
	GlobalLogBroker = nil

	// 1. Verify handleDashboardUI omits Clear-Site-Data while preserving no-store headers.
	uiReq := httptest.NewRequest(http.MethodGet, "/", nil)
	uiRec := httptest.NewRecorder()
	handleDashboardUI(uiRec, uiReq)
	if uiRec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from handleDashboardUI, got %d", uiRec.Code)
	}
	if got := uiRec.Header().Get("Clear-Site-Data"); got != "" {
		t.Errorf("Expected empty Clear-Site-Data header, got %q", got)
	}
	if !strings.Contains(uiRec.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("Expected Cache-Control to include no-store, got %q", uiRec.Header().Get("Cache-Control"))
	}

	// 2. Verify handleSSE emits X-Accel-Buffering: no and omits hop-by-hop Connection: keep-alive.
	sseCtx, cancelSSE := context.WithCancel(context.Background())
	cancelSSE() // Allows handleSSE to set headers, send initial state, and exit immediately.
	sseReq := httptest.NewRequest(http.MethodGet, "/dashboard/sse", nil).WithContext(sseCtx)
	sseRec := httptest.NewRecorder()
	handleSSE(sseRec, sseReq)
	if got := sseRec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("Expected X-Accel-Buffering=no, got %q", got)
	}
	if got := sseRec.Header().Get("Connection"); got != "" {
		t.Errorf("Expected no Connection header on SSE response, got %q", got)
	}

	// 3. Verify collectDashboardSnapshot includes completed files in "Pending Recompose".
	key := "pending_recompose_visible.mkv"
	state := NewFileCacheState(key, "http://example.com/vid.mkv", "", "", "", "video/x-matroska", "", "", 1024)
	state.DiskMaterialized = true
	state.RangeSet.Add(0, 1024)
	state.SetUpstreamStatus("Pending Recompose")

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	snap := collectDashboardSnapshot()
	found := false
	for _, af := range snap.ActiveFiles {
		if af.Key == key {
			found = true
			if af.UpstreamStatus != "Pending Recompose" {
				t.Errorf("Expected UpstreamStatus='Pending Recompose', got %q", af.UpstreamStatus)
			}
			break
		}
	}
	if !found {
		t.Fatalf("Expected completed file in 'Pending Recompose' state to appear in dashboard ActiveFiles")
	}
}

func TestFindLargestGapMidpointAlignsToChunkBoundary(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)
	GlobalConfig = &Config{
		TmpFileMaxBytes: 512 * 1024,
	}

	const chunkSize = int64(64 * 1024)
	const totalSize = int64(512 * 1024)

	// 1. With ScratchChunkManager: gap [20 KiB, 512 KiB) snaps midpoint (266 KiB) to nearest 64 KiB multiple (256 KiB).
	state := &FileCacheState{
		Key:       "align_test.mkv",
		TotalSize: totalSize,
		RangeSet:  NewRangeSet(),
	}
	state.RangeSet.Add(0, 20*1024)
	cm := NewScratchChunkManagerWithTarget(state, nil, chunkSize)
	defer cm.Close()
	state.ChunkManager = cm

	mid, gs, ge, ok := findLargestGapMidpoint(state)
	if !ok {
		t.Fatalf("Expected ok=true for [20 KiB, 512 KiB) gap")
	}
	if gs != 20*1024 || ge != totalSize {
		t.Fatalf("Unexpected gap [%d, %d), want [%d, %d)", gs, ge, 20*1024, totalSize)
	}
	if mid%chunkSize != 0 || mid <= gs || mid >= ge {
		t.Fatalf("Expected chunk-aligned midpoint multiple of %d inside (%d, %d), got %d", chunkSize, gs, ge, mid)
	}
	if mid != 256*1024 {
		t.Fatalf("Expected nearest chunk boundary 262144 (256 KiB), got %d", mid)
	}

	// 2. Sub-chunk gap [10 KiB, 50 KiB) with chunkSize=64 KiB has no interior chunk boundary: falls back to raw midpoint 30 KiB.
	subState := &FileCacheState{
		Key:       "subchunk_align_test.mkv",
		TotalSize: 60 * 1024,
		RangeSet:  NewRangeSet(),
	}
	subState.RangeSet.Add(0, 10*1024)
	subState.RangeSet.Add(50*1024, 60*1024)
	subCM := NewScratchChunkManagerWithTarget(subState, nil, chunkSize)
	defer subCM.Close()
	subState.ChunkManager = subCM

	subMid, sgs, sge, sok := findLargestGapMidpoint(subState)
	if !sok || sgs != 10*1024 || sge != 50*1024 || subMid != 30*1024 {
		t.Fatalf("Expected raw midpoint fallback 30720 for [10240, 51200), got mid=%d gap=[%d,%d) ok=%v", subMid, sgs, sge, sok)
	}

	// 3. Without ScratchChunkManager (cm == nil): returns raw midpoint (266 KiB).
	noCMState := &FileCacheState{
		Key:       "nocm_align_test.mkv",
		TotalSize: totalSize,
		RangeSet:  NewRangeSet(),
	}
	noCMState.RangeSet.Add(0, 20*1024)
	rawMid, _, _, rawOK := findLargestGapMidpoint(noCMState)
	if !rawOK || rawMid != (20*1024+totalSize)/2 {
		t.Fatalf("Expected raw midpoint %d without ChunkManager, got %d (ok=%v)", (20*1024+totalSize)/2, rawMid, rawOK)
	}
}

func TestDashboardSnapshotIncludesStorageTiers(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)

	tempDir := t.TempDir()
	cacheDir := filepath.Join(tempDir, "cache")
	tmpDir := filepath.Join(tempDir, "tmp")
	stateDir := filepath.Join(tempDir, "state")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(tmpDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	GlobalConfig = &Config{
		StorageBackend:   "hybrid",
		CacheDir:         cacheDir,
		TmpDir:           tmpDir,
		StateDir:         stateDir,
		CacheMaxBytes:    10 * 1024 * 1024,
		TmpFileMaxBytes:  4 * 1024 * 1024,
		CacheMaxIdleAge:  24 * time.Hour,
		S3Config: S3Config{
			Bucket:   "dash-tier-bucket",
			Endpoint: "https://s3.example.com",
			MaxBytes: 50 * 1024 * 1024,
		},
	}
	InitRegistry()
	idx := NewCacheIndex(filepath.Join(stateDir, "s3_index.json"))
	idx.RecordCommitForTier("s3_item.mkv", FileMeta{TotalSize: 12345, URL: "http://origin/s3.mkv"}, IndexTierS3)
	idx.RecordCommitForTier("disk_item.mkv", FileMeta{TotalSize: 6789, URL: "http://origin/disk.mkv"}, IndexTierDisk)
	GlobalCacheIndex = idx
	GlobalS3Index = idx

	activeTmpState := NewFileCacheState("active_tmp.mkv", "http://origin/active.mkv", "", filepath.Join(tmpDir, "active_tmp.mkv.part"), "", "video/mp4", "", "", 4096)
	activeTmpState.DiskMaterialized = true
	activeTmpState.DiskRangeSet.Add(0, 1024)
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[activeTmpState.Key] = activeTmpState
	GlobalRegistry.Lock.Unlock()

	snap := collectDashboardSnapshot()
	st := snap.Storage
	if !st.CacheEnabled || !st.S3Enabled {
		t.Fatalf("Expected hybrid snapshot to enable both Cache and S3 tiers, got %+v", st)
	}
	if st.TmpObjects != 1 || st.TmpUsedBytes != 1024 {
		t.Errorf("Expected TmpObjects=1 and TmpUsedBytes=1024, got %+v", st)
	}
	if st.TmpDir != tmpDir || st.CacheDir != cacheDir {
		t.Errorf("Unexpected tier dirs: tmp=%q (want %q), cache=%q (want %q)", st.TmpDir, tmpDir, st.CacheDir, cacheDir)
	}
	if st.TmpFileMaxBytes != 4*1024*1024 || st.CacheMaxBytes != 10*1024*1024 || st.S3MaxBytes != 50*1024*1024 {
		t.Errorf("Unexpected tier limits: %+v", st)
	}
	if st.S3Bucket != "dash-tier-bucket" || st.S3UsedBytes != 12345 || st.S3Objects != 1 {
		t.Errorf("Unexpected S3 usage/bucket fields: %+v", st)
	}
	if st.CacheObjects != 1 || st.CacheMaxIdleAgeSec != int64((24*time.Hour).Seconds()) {
		t.Errorf("Unexpected cache objects/TTL fields: %+v", st)
	}
}

type slowMockChunkTarget struct {
	name      string
	chunkSize int64
	started   chan struct{}
	startOnce sync.Once
	aborted   atomic.Bool
	cleanedUp atomic.Bool
}

func (t *slowMockChunkTarget) Name() string     { return t.name }
func (t *slowMockChunkTarget) IsSequential() bool { return false }
func (t *slowMockChunkTarget) Concurrency() int   { return 1 }
func (t *slowMockChunkTarget) SupportsMultipart(totalSize, configuredChunkSize int64) (int64, bool) {
	return t.chunkSize, true
}
func (t *slowMockChunkTarget) PushChunk(ctx context.Context, key string, chunkIndex int, chunkStart int64, r io.Reader, size int64) error {
	t.startOnce.Do(func() { close(t.started) })
	<-ctx.Done()
	t.aborted.Store(true)
	return ctx.Err()
}
func (t *slowMockChunkTarget) Commit(ctx context.Context, key string, isMultipart bool, totalParts int, localPartPath string, meta FileMeta, onProgress CommitProgressFunc) error {
	return nil
}
func (t *slowMockChunkTarget) ReadCommittedChunk(ctx context.Context, key string, chunkIndex int, chunkStart, offsetInChunk, length int64) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (t *slowMockChunkTarget) Cleanup(ctx context.Context, key string) error {
	t.cleanedUp.Store(true)
	return nil
}

func TestSlowTargetUploadYieldsPrehydrationThenUnpairsToRingBuffer(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)
	setGlobalStorageHealth(nil)

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		StorageBackend:            "s3",
		TmpDir:                    filepath.Join(tempDir, "tmp"),
		StateDir:                  filepath.Join(tempDir, "state"),
		TmpFileMaxBytes:           64 * 1024,
		PrehydrateYieldBackoff:    5 * time.Second,
		EnableEpisodePrehydration: true,
		S3Config: S3Config{
			Bucket:            "slow-s3-bucket",
			ChunkSize:         16 * 1024,
			UploadConcurrency: 1,
		},
	}
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	_ = os.MkdirAll(GlobalConfig.StateDir, 0755)
	GlobalStorage = nil
	InitRegistry()
	t.Cleanup(func() {
		if GlobalRegistry != nil {
			GlobalRegistry.Stop()
		}
	})
	if curHealth := getGlobalStorageHealth(); curHealth != nil {
		curHealth.Stop()
	}
	setGlobalStorageHealth(nil)
	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		tierContentionUntil: make(map[string]time.Time),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		pollCh:              make(chan struct{}, 1),
	}

	// 1. Register an active competing S3 prehydration that must yield under contention.
	preCtx, preCancel := context.WithCancel(context.Background())
	defer preCancel()
	preKey := "competing_prehydrate.mkv"
	preState := NewFileCacheState(preKey, "http://origin/pre.mkv", "", filepath.Join(GlobalConfig.TmpDir, preKey+".part"), "", "video/mp4", "", "", 128*1024)
	preState.Lock.Lock()
	preState.TargetTier = TierS3
	preState.IsPrehydrating = true
	preState.WriterActive = true
	preState.Cancel = preCancel
	preState.Lock.Unlock()
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[preKey] = preState
	GlobalRegistry.Lock.Unlock()
	GlobalPrehydrationManager.mu.Lock()
	GlobalPrehydrationManager.activePrehydrations[preKey] = true
	GlobalPrehydrationManager.mu.Unlock()

	// 2. Set up live stream hitting scratch budget with a starving client and stalled S3 upload.
	liveKey := "live_slow_s3.mkv"
	livePart := filepath.Join(GlobalConfig.TmpDir, liveKey+".part")
	if err := os.WriteFile(livePart, make([]byte, 48*1024), 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}
	liveState := NewFileCacheState(liveKey, "http://origin/live.mkv", "", livePart, "", "video/mp4", "", "", 128*1024)
	liveState.Lock.Lock()
	liveState.TargetTier = TierS3
	liveState.DiskMaterialized = true
	liveState.RangeSet.Add(0, 48*1024)
	liveState.DiskRangeSet.Add(0, 48*1024)
	liveState.ClientOffsets["viewer-1"] = 32 * 1024
	liveState.Starving["viewer-1"] = 32 * 1024
	liveState.StarvingSince["viewer-1"] = time.Now()
	liveState.Lock.Unlock()

	slowTarget := &slowMockChunkTarget{
		name:      "s3",
		chunkSize: 16 * 1024,
		started:   make(chan struct{}),
	}
	cm := NewScratchChunkManagerWithTarget(liveState, slowTarget, 16*1024)
	defer cm.Close()
	cm.maxInFlightBytes = 32 * 1024
	cm.starveYieldDelay = 40 * time.Millisecond
	cm.starveDegradeDelay = 120 * time.Millisecond
	liveState.Lock.Lock()
	liveState.ChunkManager = cm
	liveState.Lock.Unlock()

	cm.AddInFlightBytes(48 * 1024)
	cm.CheckAndTriggerUploads()
	<-slowTarget.started

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer waitCancel()
	if err := cm.WaitScratchBudget(waitCtx); err != nil {
		t.Fatalf("Expected WaitScratchBudget to degrade to ring buffer and unblock, got: %v", err)
	}

	if !cm.IsRingBufferMode() || liveState.GetTargetTier() != TierTmp {
		t.Fatalf("Expected stream to unpair into TierTmp ring buffer (ring=%v, tier=%s)", cm.IsRingBufferMode(), liveState.GetTargetTier())
	}
	preState.Lock.Lock()
	preYielding := preState.IsYielding
	preState.Lock.Unlock()
	if !preYielding || preCtx.Err() == nil {
		t.Fatalf("Expected competing S3 prehydration to yield (yielding=%v, ctxErr=%v)", preYielding, preCtx.Err())
	}
	if !GlobalPrehydrationManager.IsTierUnderContention("s3") || GlobalPrehydrationManager.CanStartPrehydration("http://origin/other.mkv") {
		t.Fatalf("Expected S3 tier contention to temporarily block new S3 prehydrations")
	}

	if !waitForPhase1Condition(2*time.Second, func() bool {
		return slowTarget.aborted.Load() && slowTarget.cleanedUp.Load()
	}) {
		t.Fatalf("Expected slow in-flight PushChunk to abort (%v) and trigger Cleanup (%v)",
			slowTarget.aborted.Load(), slowTarget.cleanedUp.Load())
	}
	if !IsS3Healthy() {
		t.Fatalf("Expected S3 circuit breaker to stay closed during slow-upload unpair")
	}
}

type blockingThenReadyChunkTarget struct {
	name      string
	chunkSize int64
	block     atomic.Bool
	started   chan struct{}
	startOnce sync.Once
	aborted   atomic.Bool
	pushedCh1 atomic.Bool
}

func (t *blockingThenReadyChunkTarget) Name() string     { return t.name }
func (t *blockingThenReadyChunkTarget) IsSequential() bool { return false }
func (t *blockingThenReadyChunkTarget) Concurrency() int   { return 1 }
func (t *blockingThenReadyChunkTarget) SupportsMultipart(totalSize, configuredChunkSize int64) (int64, bool) {
	return t.chunkSize, true
}
func (t *blockingThenReadyChunkTarget) PushChunk(ctx context.Context, key string, chunkIndex int, chunkStart int64, r io.Reader, size int64) error {
	if t.block.Load() {
		t.startOnce.Do(func() { close(t.started) })
		<-ctx.Done()
		t.aborted.Store(true)
		return ctx.Err()
	}
	t.pushedCh1.Store(true)
	return nil
}
func (t *blockingThenReadyChunkTarget) Commit(ctx context.Context, key string, isMultipart bool, totalParts int, localPartPath string, meta FileMeta, onProgress CommitProgressFunc) error {
	return nil
}
func (t *blockingThenReadyChunkTarget) ReadCommittedChunk(ctx context.Context, key string, chunkIndex int, chunkStart, offsetInChunk, length int64) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (t *blockingThenReadyChunkTarget) Cleanup(ctx context.Context, key string) error { return nil }

func TestHealthyPacingDoesNotFlapPrehydrationWhileLowReadAheadPausesPrehydrationUploads(t *testing.T) {
	saveAndRestoreResilienceGlobals(t)
	setGlobalStorageHealth(nil)

	tempDir := t.TempDir()
	GlobalConfig = &Config{
		StorageBackend:            "s3",
		TmpDir:                    filepath.Join(tempDir, "tmp"),
		StateDir:                  filepath.Join(tempDir, "state"),
		TmpFileMaxBytes:           64 * 1024,
		PrehydrateYieldBackoff:    5 * time.Second,
		EnableEpisodePrehydration: true,
		S3Config: S3Config{
			Bucket:            "pacing-s3-bucket",
			ChunkSize:         16 * 1024,
			UploadConcurrency: 1,
		},
	}
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)
	_ = os.MkdirAll(GlobalConfig.StateDir, 0755)
	GlobalStorage = nil
	InitRegistry()
	t.Cleanup(func() {
		if GlobalRegistry != nil {
			GlobalRegistry.Stop()
		}
	})
	if curHealth := getGlobalStorageHealth(); curHealth != nil {
		curHealth.Stop()
	}
	setGlobalStorageHealth(nil)
	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		tierContentionUntil: make(map[string]time.Time),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		pollCh:              make(chan struct{}, 1),
	}

	// 1. Start a competing S3 prehydration with an active in-flight chunk upload and 1 already-uploaded chunk.
	preKey := "pacing_prehydrate.mkv"
	prePart := filepath.Join(GlobalConfig.TmpDir, preKey+".part")
	if err := os.WriteFile(prePart, make([]byte, 32*1024), 0644); err != nil {
		t.Fatalf("WriteFile prePart error: %v", err)
	}
	preCtx, preCancel := context.WithCancel(context.Background())
	defer preCancel()
	preState := NewFileCacheState(preKey, "http://origin/pre2.mkv", "", prePart, "", "video/mp4", "", "", 128*1024)
	preState.Lock.Lock()
	preState.TargetTier = TierS3
	preState.DiskMaterialized = true
	preState.IsPrehydrating = true
	preState.WriterActive = true
	preState.Cancel = preCancel
	preState.RangeSet.Add(0, 16*1024)
	preState.DiskRangeSet.Add(0, 16*1024)
	preState.Lock.Unlock()

	preTarget := &blockingThenReadyChunkTarget{
		name:      "s3",
		chunkSize: 16 * 1024,
		started:   make(chan struct{}),
	}
	preTarget.block.Store(true)
	preCM := NewScratchChunkManagerWithTarget(preState, preTarget, 16*1024)
	defer preCM.Close()
	preCM.mu.Lock()
	preCM.uploaded[1] = true
	preCM.mu.Unlock()
	preState.Lock.Lock()
	preState.ChunkManager = preCM
	preState.Lock.Unlock()
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[preKey] = preState
	GlobalRegistry.Lock.Unlock()
	GlobalPrehydrationManager.mu.Lock()
	GlobalPrehydrationManager.activePrehydrations[preKey] = true
	GlobalPrehydrationManager.mu.Unlock()

	preCM.CheckAndTriggerUploads()
	<-preTarget.started

	// 2. Live stream blocked in WaitScratchBudget with generous 64KB buffer ahead (lowReadAhead=8KB):
	// must NOT flap/yield the prehydration during normal pacing.
	liveKey := "healthy_pacing_live.mkv"
	livePart := filepath.Join(GlobalConfig.TmpDir, liveKey+".part")
	if err := os.WriteFile(livePart, make([]byte, 64*1024), 0644); err != nil {
		t.Fatalf("WriteFile livePart error: %v", err)
	}
	liveState := NewFileCacheState(liveKey, "http://origin/live2.mkv", "", livePart, "", "video/mp4", "", "", 128*1024)
	liveState.Lock.Lock()
	liveState.TargetTier = TierS3
	liveState.DiskMaterialized = true
	liveState.RangeSet.Add(0, 64*1024)
	liveState.DiskRangeSet.Add(0, 64*1024)
	liveState.ClientOffsets["viewer-1"] = 0
	liveState.Lock.Unlock()

	liveTarget := &slowMockChunkTarget{
		name:      "s3",
		chunkSize: 16 * 1024,
		started:   make(chan struct{}),
	}
	cm := NewScratchChunkManagerWithTarget(liveState, liveTarget, 16*1024)
	defer cm.Close()
	cm.maxInFlightBytes = 32 * 1024 // lowReadAhead = 8KB
	cm.starveYieldDelay = 40 * time.Millisecond
	cm.starveDegradeDelay = 5 * time.Second
	liveState.Lock.Lock()
	liveState.ChunkManager = cm
	liveState.Lock.Unlock()
	cm.AddInFlightBytes(32 * 1024)

	healthyWaitCtx, healthyWaitCancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	_ = cm.WaitScratchBudget(healthyWaitCtx)
	healthyWaitCancel()

	preState.Lock.Lock()
	yieldedDuringHealthy := preState.IsYielding
	preState.Lock.Unlock()
	if yieldedDuringHealthy || preCtx.Err() != nil || GlobalPrehydrationManager.IsTierUnderContention("s3") || preTarget.aborted.Load() {
		t.Fatalf("Expected healthy read-ahead pacing NOT to yield/abort prehydration (yielding=%v, preCtx=%v, contended=%v, aborted=%v)",
			yieldedDuringHealthy, preCtx.Err(), GlobalPrehydrationManager.IsTierUnderContention("s3"), preTarget.aborted.Load())
	}

	// 3. Advance reader so read-ahead shrinks to 4KB (< 8KB lowReadAhead), WITHOUT starving:
	// must proactively yield prehydration AND PauseUploads() immediately while keeping live stream paired.
	liveState.Lock.Lock()
	liveState.ClientOffsets["viewer-1"] = 60 * 1024
	liveState.Lock.Unlock()

	lowBufCtx, lowBufCancel := context.WithTimeout(context.Background(), 160*time.Millisecond)
	_ = cm.WaitScratchBudget(lowBufCtx)
	lowBufCancel()

	preState.Lock.Lock()
	yieldedOnLowBuf := preState.IsYielding
	preState.Lock.Unlock()
	if !yieldedOnLowBuf || preCtx.Err() == nil || !GlobalPrehydrationManager.IsTierUnderContention("s3") {
		t.Fatalf("Expected low read-ahead to proactively yield prehydration before starvation (yielding=%v, preCtx=%v, contended=%v)",
			yieldedOnLowBuf, preCtx.Err(), GlobalPrehydrationManager.IsTierUnderContention("s3"))
	}
	if !waitForPhase1Condition(2*time.Second, func() bool {
		return preTarget.aborted.Load()
	}) {
		t.Fatalf("Expected yielded prehydration in-flight upload to abort via PauseUploads")
	}
	if cm.IsRingBufferMode() {
		t.Fatalf("Expected live stream to stay paired to S3 during proactive yield before starvation delay")
	}
	if !preCM.IsChunkUploaded(1) {
		t.Fatalf("Expected PauseUploads to preserve previously uploaded chunks")
	}

	// 4. Verify ResumeUploads cleanly restarts prehydration uploads once S3 contention clears.
	preTarget.block.Store(false)
	preCM.ResumeUploads()
	if !waitForPhase1Condition(2*time.Second, func() bool {
		return preTarget.pushedCh1.Load()
	}) {
		t.Fatalf("Expected prehydration ResumeUploads to push chunk 0 after unpausing")
	}
}
