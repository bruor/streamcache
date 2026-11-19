package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func clearPhase0ConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"PORT",
		"STORAGE_BACKEND",
		"CACHE_DIR",
		"TMP_DIR",
		"STATE_DIR",
		"CACHE_MAX_BYTES",
		"CACHE_MAX_ITEM_PCT",
		"CACHE_MIN_DISK_FREE_BYTES",
		"NAS_SEQUENTIAL_STAGE",
		"TMP_FILE_MAX_BYTES",
		"TMP_MIN_DISK_FREE_BYTES",
		"CACHE_MAX_IDLE_AGE",
		"CACHE_EVICT_INTERVAL",
		"HYBRID_DEMOTE_HIGH_WATERMARK",
		"HYBRID_DEMOTE_LOW_WATERMARK",
		"HYBRID_EAGER_S3_BACKUP",
		"S3_UPLOAD_CONCURRENCY",
		"S3_RECOMPOSE_CONCURRENCY",
		"UPSTREAM_USER_AGENT_MODE",
		"BLOCK_SIZE",
		"SCHEDULER_MIN_BYTES",
		"MIN_DISK_FREE_BYTES",
		"S3_SCRATCH_MAX_BYTES",
		"S3_SCRATCH_MIN_FREE_BYTES",
		"S3_MAX_IDLE_AGE",
		"S3_EVICT_INTERVAL",
	}
	for _, k := range keys {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

func TestConfigPhase0EnvParsingAndLegacyWarnings(t *testing.T) {
	clearPhase0ConfigEnv(t)

	if warnings := detectLegacyConfigWarnings(); len(warnings) != 0 {
		t.Fatalf("Expected 0 legacy warnings on clean env, got %v", warnings)
	}

	cfg := parseConfigFromEnv()
	if cfg.GetStorageBackend() != "disk" {
		t.Errorf("Expected default STORAGE_BACKEND='disk', got %q", cfg.GetStorageBackend())
	}
	if cfg.GetTmpFileMaxBytes() != 512*1024*1024 {
		t.Errorf("Expected default TMP_FILE_MAX_BYTES=512MB, got %d", cfg.GetTmpFileMaxBytes())
	}
	if cfg.GetTmpMinDiskFreeBytes() != 1*1024*1024*1024 {
		t.Errorf("Expected default TMP_MIN_DISK_FREE_BYTES=1GB, got %d", cfg.GetTmpMinDiskFreeBytes())
	}
	if cfg.GetCacheMinDiskFreeBytes() != 10*1024*1024*1024 {
		t.Errorf("Expected default CACHE_MIN_DISK_FREE_BYTES=10GB, got %d", cfg.GetCacheMinDiskFreeBytes())
	}
	if cfg.GetCacheMaxIdleAge() != 720*time.Hour {
		t.Errorf("Expected default CACHE_MAX_IDLE_AGE=720h, got %s", cfg.GetCacheMaxIdleAge())
	}
	if cfg.GetCacheEvictInterval() != 6*time.Hour {
		t.Errorf("Expected default CACHE_EVICT_INTERVAL=6h, got %s", cfg.GetCacheEvictInterval())
	}
	s3Cfg := cfg.GetS3Config()
	if s3Cfg.UploadConcurrency != 4 || s3Cfg.RecomposeConcurrency != 6 {
		t.Errorf("Expected default S3 concurrency (4, 6), got (%d, %d)", s3Cfg.UploadConcurrency, s3Cfg.RecomposeConcurrency)
	}

	// Custom values & [1, 16] clamping
	t.Setenv("STORAGE_BACKEND", " S3 ")
	t.Setenv("TMP_FILE_MAX_BYTES", "256MB")
	t.Setenv("TMP_MIN_DISK_FREE_BYTES", "512MB")
	t.Setenv("CACHE_MIN_DISK_FREE_BYTES", "20GB")
	t.Setenv("CACHE_MAX_IDLE_AGE", "48h")
	t.Setenv("CACHE_EVICT_INTERVAL", "2h")
	t.Setenv("S3_UPLOAD_CONCURRENCY", "99")
	t.Setenv("S3_RECOMPOSE_CONCURRENCY", "0")

	cfg = parseConfigFromEnv()
	if cfg.GetStorageBackend() != "s3" {
		t.Errorf("Expected normalized STORAGE_BACKEND='s3', got %q", cfg.GetStorageBackend())
	}
	if cfg.GetTmpFileMaxBytes() != 256*1024*1024 {
		t.Errorf("Expected custom TMP_FILE_MAX_BYTES=256MB, got %d", cfg.GetTmpFileMaxBytes())
	}
	if cfg.GetTmpMinDiskFreeBytes() != 512*1024*1024 {
		t.Errorf("Expected custom TMP_MIN_DISK_FREE_BYTES=512MB, got %d", cfg.GetTmpMinDiskFreeBytes())
	}
	if cfg.GetCacheMinDiskFreeBytes() != 20*1024*1024*1024 {
		t.Errorf("Expected custom CACHE_MIN_DISK_FREE_BYTES=20GB, got %d", cfg.GetCacheMinDiskFreeBytes())
	}
	if cfg.GetCacheMaxIdleAge() != 48*time.Hour || cfg.GetCacheEvictInterval() != 2*time.Hour {
		t.Errorf("Unexpected cache eviction durations: %s / %s", cfg.GetCacheMaxIdleAge(), cfg.GetCacheEvictInterval())
	}
	s3Cfg = cfg.GetS3Config()
	if s3Cfg.UploadConcurrency != 16 {
		t.Errorf("Expected clamped S3_UPLOAD_CONCURRENCY=16, got %d", s3Cfg.UploadConcurrency)
	}
	if s3Cfg.RecomposeConcurrency != 6 {
		t.Errorf("Expected default fallback S3_RECOMPOSE_CONCURRENCY=6 for 0, got %d", s3Cfg.RecomposeConcurrency)
	}

	// Verify removed passthroughs do NOT silently translate values and emit warnings
	t.Setenv("BLOCK_SIZE", "4MB")
	t.Setenv("SCHEDULER_MIN_BYTES", "16MB")
	t.Setenv("MIN_DISK_FREE_BYTES", "5GB")
	t.Setenv("S3_SCRATCH_MAX_BYTES", "1GB")
	t.Setenv("S3_SCRATCH_MIN_FREE_BYTES", "256MB")
	t.Setenv("S3_MAX_IDLE_AGE", "24h")
	t.Setenv("S3_EVICT_INTERVAL", "1h")
	t.Setenv("STORAGE_BACKEND", "local")
	t.Setenv("UPSTREAM_USER_AGENT_MODE", "override")

	cfg = parseConfigFromEnv()
	if cfg.GetStorageBackend() != "local" {
		t.Errorf("Expected raw 'local' without silent translation, got %q", cfg.GetStorageBackend())
	}
	if cfg.GetUpstreamUserAgentMode() != "override" {
		t.Errorf("Expected raw 'override' without silent translation, got %q", cfg.GetUpstreamUserAgentMode())
	}

	warnings := detectLegacyConfigWarnings()
	joined := strings.Join(warnings, "\n")
	expectedNeedles := []string{
		"BLOCK_SIZE",
		"SCHEDULER_MIN_BYTES",
		"MIN_DISK_FREE_BYTES",
		"S3_SCRATCH_MAX_BYTES",
		"S3_SCRATCH_MIN_FREE_BYTES",
		"S3_MAX_IDLE_AGE",
		"S3_EVICT_INTERVAL",
		`STORAGE_BACKEND="local"`,
		`UPSTREAM_USER_AGENT_MODE="override"`,
	}
	for _, needle := range expectedNeedles {
		if !strings.Contains(joined, needle) {
			t.Errorf("Expected warning mentioning %q, got warnings:\n%s", needle, joined)
		}
	}

	t.Setenv("UPSTREAM_USER_AGENT_MODE", "disabled")
	cfg = parseConfigFromEnv()
	if cfg.GetUpstreamUserAgentMode() != "disabled" {
		t.Errorf("Expected raw 'disabled' without silent translation, got %q", cfg.GetUpstreamUserAgentMode())
	}
	joined = strings.Join(detectLegacyConfigWarnings(), "\n")
	if !strings.Contains(joined, `UPSTREAM_USER_AGENT_MODE="disabled"`) {
		t.Errorf("Expected warning mentioning disabled UA mode, got:\n%s", joined)
	}

	localBackend := NewLocalStorageBackend(t.TempDir(), t.TempDir())
	if localBackend.Type() != "disk" {
		t.Errorf("Expected LocalStorageBackend.Type()='disk', got %q", localBackend.Type())
	}
}

func TestS3ParallelRecomposeRetriesAndOrder(t *testing.T) {
	const bucket = "recompose-parallel-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	const chunkSize = int64(16 * 1024)
	const totalParts = 12
	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:             mockS3.URL(),
		Bucket:               bucket,
		Region:               "us-east-1",
		AccessKey:            "test-access",
		SecretKey:            "test-secret",
		UsePathStyle:         true,
		DisableSSL:           true,
		ChunkSize:            chunkSize,
		RecomposeConcurrency: 6,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	key := "parallel_recompose.mkv"
	fullData := make([]byte, int(chunkSize)*totalParts)
	_, _ = rand.Read(fullData)

	ctx := context.Background()
	for i := 0; i < totalParts; i++ {
		off := int64(i) * chunkSize
		if err := backend.UploadChunk(ctx, key, i, bytes.NewReader(fullData[off:off+chunkSize]), chunkSize); err != nil {
			t.Fatalf("UploadChunk %d error: %v", i, err)
		}
	}

	var part4Attempts atomic.Int32
	var curCopy atomic.Int32
	var maxCopy atomic.Int32
	var barrierOnce sync.Once
	barrierReady := make(chan struct{})

	mockS3.SetRequestHook(func(r *http.Request) int {
		if r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "" {
			cur := curCopy.Add(1)
			for {
				prev := maxCopy.Load()
				if cur <= prev || maxCopy.CompareAndSwap(prev, cur) {
					break
				}
			}
			if cur >= 3 {
				barrierOnce.Do(func() { close(barrierReady) })
			}
			select {
			case <-barrierReady:
			case <-time.After(500 * time.Millisecond):
			}
			curCopy.Add(-1)

			if r.URL.Query().Get("partNumber") == "4" {
				if part4Attempts.Add(1) <= 2 {
					return http.StatusServiceUnavailable
				}
			}
		}
		return 0
	})

	var events []CommitProgress
	progCtx := WithCommitProgress(ctx, func(stage string, done, total int) {
		events = append(events, CommitProgress{Stage: stage, Completed: done, Total: total})
	})

	if err := backend.Recompose(progCtx, key, totalParts, FileMeta{TotalSize: int64(len(fullData))}); err != nil {
		t.Fatalf("Recompose failed despite retryable 503s: %v", err)
	}

	if maxCopy.Load() < 3 {
		t.Errorf("Expected >= 3 concurrent UploadPartCopy requests, max observed=%d", maxCopy.Load())
	}
	if part4Attempts.Load() != 3 {
		t.Errorf("Expected part 4 to take 3 attempts (2 retries), got %d", part4Attempts.Load())
	}

	recompDone := 0
	for _, ev := range events {
		if ev.Stage == "recomposing" {
			if ev.Total != totalParts {
				t.Fatalf("Unexpected recomposing total %d", ev.Total)
			}
			if ev.Completed != recompDone {
				t.Fatalf("Expected monotonic recomposing count %d, got %d (events=%v)", recompDone, ev.Completed, events)
			}
			recompDone++
		}
	}
	if recompDone != totalParts+1 {
		t.Fatalf("Expected recomposing 0..%d events, got %d events", totalParts, recompDone)
	}

	got, ok := mockS3.GetObject(bucket + "/vod/" + key)
	if !ok {
		t.Fatalf("Expected vod/%s to exist in MockS3Server", key)
	}
	if !bytes.Equal(got, fullData) {
		t.Fatalf("Recomposed payload corrupted or out-of-order (%d bytes vs %d bytes)", len(got), len(fullData))
	}
	if mockS3.ActiveUploads() != 0 {
		t.Errorf("Expected 0 active multipart uploads, got %d", mockS3.ActiveUploads())
	}

	// Subtest B: canceled request context during UploadPartCopy must still abort multipart upload via detached 15s ctx.
	failKey := "cancel_abort.mkv"
	for i := 0; i < 4; i++ {
		if err := backend.UploadChunk(ctx, failKey, i, bytes.NewReader(fullData[:chunkSize]), chunkSize); err != nil {
			t.Fatalf("UploadChunk %d error: %v", i, err)
		}
	}

	copyStarted := make(chan struct{})
	var startOnce sync.Once
	mockS3.SetRequestHook(func(r *http.Request) int {
		if r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "" {
			startOnce.Do(func() { close(copyStarted) })
			<-r.Context().Done()
			return 0
		}
		return 0
	})

	cancelCtx, cancel := context.WithCancel(context.Background())
	recompErrCh := make(chan error, 1)
	go func() {
		recompErrCh <- backend.Recompose(cancelCtx, failKey, 4, FileMeta{TotalSize: 4 * chunkSize})
	}()

	<-copyStarted
	cancel()

	select {
	case err := <-recompErrCh:
		if err == nil {
			t.Fatalf("Expected Recompose error after context cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Timed out waiting for canceled Recompose to unwind")
	}

	if mockS3.ActiveUploads() != 0 {
		t.Errorf("Expected detached 15s AbortMultipartUpload to clean up multipart upload after cancel, ActiveUploads=%d", mockS3.ActiveUploads())
	}
}

func TestS3UploadChunkStreamingAndConcurrencyCap(t *testing.T) {
	const bucket = "upload-stream-cap-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	const chunkSize = int64(16 * 1024)
	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:          mockS3.URL(),
		Bucket:            bucket,
		Region:            "us-east-1",
		AccessKey:         "test-access",
		SecretKey:         "test-secret",
		UsePathStyle:      true,
		DisableSSL:        true,
		ChunkSize:         chunkSize,
		UploadConcurrency: 3,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	tempDir := t.TempDir()
	key := "stream_chunk.mkv"
	fileData := make([]byte, chunkSize*3)
	_, _ = rand.Read(fileData)
	scratchPath := filepath.Join(tempDir, "stream_src.bin")
	if err := os.WriteFile(scratchPath, fileData, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	f, err := os.Open(scratchPath)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	defer f.Close()

	ctx := context.Background()
	// 1. SectionReader (io.ReadSeeker + io.ReaderAt with exact length)
	if err := backend.UploadChunk(ctx, key, 0, io.NewSectionReader(f, 0, chunkSize), chunkSize); err != nil {
		t.Fatalf("UploadChunk SectionReader error: %v", err)
	}
	// 2. Positioned *os.File with trailing bytes remaining > chunkSize
	if _, err := f.Seek(chunkSize, io.SeekStart); err != nil {
		t.Fatalf("Seek error: %v", err)
	}
	if err := backend.UploadChunk(ctx, key, 1, f, chunkSize); err != nil {
		t.Fatalf("UploadChunk positioned *os.File error: %v", err)
	}
	// 3. Non-seekable io.Reader fallback
	if err := backend.UploadChunk(ctx, key, 2, io.NopCloser(bytes.NewReader(fileData[chunkSize*2:])), chunkSize); err != nil {
		t.Fatalf("UploadChunk non-seekable io.Reader error: %v", err)
	}

	for i := 0; i < 3; i++ {
		rc, err := backend.OpenChunkRange(ctx, key, i, 0, chunkSize)
		if err != nil {
			t.Fatalf("OpenChunkRange(%d) error: %v", i, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("ReadAll chunk %d error: %v", i, err)
		}
		off := int64(i) * chunkSize
		if !bytes.Equal(got, fileData[off:off+chunkSize]) {
			t.Fatalf("Chunk %d data mismatch", i)
		}
	}

	// Verify ScratchChunkManager bounds concurrent PutObject uploads to UploadConcurrency (3)
	capKey := "upload_cap.mkv"
	const capParts = 8
	capData := make([]byte, chunkSize*capParts)
	_, _ = rand.Read(capData)
	partPath := filepath.Join(tempDir, capKey+".part")
	if err := os.WriteFile(partPath, capData, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	var curUploads atomic.Int32
	var maxUploads atomic.Int32
	mockS3.SetRequestHook(func(r *http.Request) int {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/chunks/") {
			cur := curUploads.Add(1)
			for {
				prev := maxUploads.Load()
				if cur <= prev || maxUploads.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			curUploads.Add(-1)
		}
		return 0
	})

	state := &FileCacheState{
		Key:               capKey,
		URL:               "http://example.com/" + capKey,
		Path:              filepath.Join(tempDir, capKey),
		PartPath:          partPath,
		MetaPath:          partPath + ".json",
		TotalSize:         int64(len(capData)),
		RangeSet:          NewRangeSet(),
		DiskRangeSet:      NewRangeSet(),
		Clients:           make(map[string]chan struct{}),
		TargetTier:        TierS3,
		UseScratchManager: true,
	}
	cm := NewScratchChunkManager(state, backend, chunkSize)
	defer cm.Close()
	state.ChunkManager = cm

	state.RangeSet.Add(0, int64(len(capData)))
	if err := cm.WaitAndRecompose(ctx); err != nil {
		t.Fatalf("WaitAndRecompose error: %v", err)
	}

	if maxUploads.Load() > 3 {
		t.Errorf("Expected max concurrent chunk uploads <= 3, got %d", maxUploads.Load())
	}
	if maxUploads.Load() < 2 {
		t.Errorf("Expected concurrent chunk uploads >= 2 with 8 parts and cap 3, got %d", maxUploads.Load())
	}
}

func TestS3DeleteChunksAndStaleChunkSweep(t *testing.T) {
	const bucket = "chunk-cleanup-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	const chunkSize = int64(8 * 1024)
	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       bucket,
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    chunkSize,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	oldReg := GlobalRegistry
	oldStorage := GlobalStorage
	oldPM := GlobalPrehydrationManager
	oldBroker := GlobalLogBroker
	GlobalRegistry = &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
	}
	GlobalStorage = backend
	GlobalPrehydrationManager = nil
	GlobalLogBroker = nil
	defer func() {
		GlobalRegistry = oldReg
		GlobalStorage = oldStorage
		GlobalPrehydrationManager = oldPM
		GlobalLogBroker = oldBroker
	}()

	ctx := context.Background()
	chunkPayload := bytes.Repeat([]byte("A"), int(chunkSize))

	if err := backend.UploadChunk(ctx, "orphan_old.mkv", 0, bytes.NewReader(chunkPayload), chunkSize); err != nil {
		t.Fatal(err)
	}
	if err := backend.UploadChunk(ctx, "orphan_old.mkv", 1, bytes.NewReader(chunkPayload), chunkSize); err != nil {
		t.Fatal(err)
	}
	if err := backend.UploadChunk(ctx, "active_old.mkv", 0, bytes.NewReader(chunkPayload), chunkSize); err != nil {
		t.Fatal(err)
	}
	if err := backend.UploadChunk(ctx, "recent.mkv", 0, bytes.NewReader(chunkPayload), chunkSize); err != nil {
		t.Fatal(err)
	}

	staleTime := time.Now().Add(-25 * time.Hour)
	freshTime := time.Now().Add(-2 * time.Hour)
	mockS3.SetObjectModTime(bucket, "chunks/orphan_old.mkv/chunk_00000.bin", staleTime)
	mockS3.SetObjectModTime(bucket, "chunks/orphan_old.mkv/chunk_00001.bin", staleTime)
	mockS3.SetObjectModTime(bucket, "chunks/active_old.mkv/chunk_00000.bin", staleTime)
	mockS3.SetObjectModTime(bucket, "chunks/recent.mkv/chunk_00000.bin", freshTime)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files["active_old.mkv"] = &FileCacheState{Key: "active_old.mkv"}
	GlobalRegistry.Lock.Unlock()

	deleted, err := backend.SweepStaleChunks(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("SweepStaleChunks error: %v", err)
	}
	if deleted != 2 {
		t.Errorf("Expected 2 stale chunks deleted for orphan_old.mkv, got %d", deleted)
	}
	if mockS3.HasObject(bucket, "chunks/orphan_old.mkv/chunk_00000.bin") || mockS3.HasObject(bucket, "chunks/orphan_old.mkv/chunk_00001.bin") {
		t.Errorf("Expected orphan_old.mkv chunks to be deleted")
	}
	if !mockS3.HasObject(bucket, "chunks/active_old.mkv/chunk_00000.bin") {
		t.Errorf("Expected active_old.mkv chunk to be preserved while key is in use")
	}
	if !mockS3.HasObject(bucket, "chunks/recent.mkv/chunk_00000.bin") {
		t.Errorf("Expected recent.mkv chunk (<24h) to be preserved")
	}

	GlobalRegistry.Lock.Lock()
	delete(GlobalRegistry.Files, "active_old.mkv")
	GlobalRegistry.Lock.Unlock()

	if err := backend.DeleteChunks(ctx, "active_old.mkv"); err != nil {
		t.Fatalf("DeleteChunks error: %v", err)
	}
	if mockS3.HasObject(bucket, "chunks/active_old.mkv/chunk_00000.bin") {
		t.Errorf("Expected DeleteChunks to remove active_old.mkv chunks")
	}
	if !mockS3.HasObject(bucket, "chunks/recent.mkv/chunk_00000.bin") {
		t.Errorf("Expected DeleteChunks not to affect unrelated keys")
	}

	// CancelCoordinator with zero remaining clients must trigger async DeleteChunks
	tempDir := t.TempDir()
	cancelKey := "cancel_async.mkv"
	if err := backend.UploadChunk(ctx, cancelKey, 0, bytes.NewReader(chunkPayload), chunkSize); err != nil {
		t.Fatal(err)
	}
	partPath := filepath.Join(tempDir, cancelKey+".part")
	metaPath := partPath + ".json"
	_ = os.WriteFile(partPath, chunkPayload, 0644)
	_ = os.WriteFile(metaPath, []byte("{}"), 0644)

	state := NewFileCacheState(cancelKey, "http://example.com/"+cancelKey, filepath.Join(tempDir, cancelKey), partPath, metaPath, "video/x-matroska", "", "", chunkSize*2)
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[cancelKey] = state
	GlobalRegistry.Lock.Unlock()

	CancelCoordinator(state)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !mockS3.HasObject(bucket, "chunks/cancel_async.mkv/chunk_00000.bin") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if mockS3.HasObject(bucket, "chunks/cancel_async.mkv/chunk_00000.bin") {
		t.Errorf("Expected CancelCoordinator to asynchronously delete staging chunks for %s", cancelKey)
	}
}

func TestPreEvictSkipsActiveDiskHITConnection(t *testing.T) {
	tmpDir := t.TempDir()

	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldMaxBytes := GlobalConfig.CacheMaxBytes
	oldMinFree := GlobalConfig.CacheMinDiskFreeBytes
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.CacheMaxBytes = 150
	GlobalConfig.CacheMinDiskFreeBytes = 0
	GlobalConfig.Unlock()
	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.CacheMaxBytes = oldMaxBytes
		GlobalConfig.CacheMinDiskFreeBytes = oldMinFree
		GlobalConfig.Unlock()
	}()

	oldReg := GlobalRegistry
	GlobalRegistry = &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
	}
	atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, 200)
	defer func() {
		GlobalRegistry = oldReg
	}()

	writeCached := func(name string, age time.Duration) {
		p := filepath.Join(tmpDir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 100), 0644); err != nil {
			t.Fatal(err)
		}
		meta := FileMeta{
			URL:           "http://example.com/" + name,
			TotalSize:     100,
			Completed:     true,
			LastValidated: time.Now().Add(-age).Unix(),
		}
		b, _ := json.Marshal(meta)
		if err := os.WriteFile(p+".json", b, 0644); err != nil {
			t.Fatal(err)
		}
	}

	// active_hit.mkv is OLDER (-2h) than idle_older.mkv (-1h), so LRU would normally pick it first,
	// but it has an active ClientConnection (disk cache HIT) and must not be evicted.
	writeCached("active_hit.mkv", 2*time.Hour)
	writeCached("idle_older.mkv", 1*time.Hour)

	GlobalRegistry.RegisterConnection("conn-hit-1", &ClientConnection{
		ID:          "conn-hit-1",
		Key:         "active_hit.mkv",
		InCache:     true,
		CacheSource: string(TierDisk),
	})

	GlobalRegistry.PreEvict(100)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		GlobalRegistry.Lock.RLock()
		evicting := GlobalRegistry.Evicting
		GlobalRegistry.Lock.RUnlock()
		if !evicting && !FileExists(filepath.Join(tmpDir, "idle_older.mkv")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if FileExists(filepath.Join(tmpDir, "idle_older.mkv")) {
		t.Fatalf("Expected inactive idle_older.mkv to be evicted")
	}
	if !FileExists(filepath.Join(tmpDir, "active_hit.mkv")) {
		t.Fatalf("Expected active_hit.mkv with active disk HIT connection to be protected from eviction")
	}

	GlobalRegistry.UnregisterConnection("conn-hit-1")
	if GlobalRegistry.IsKeyInUse("active_hit.mkv") {
		t.Errorf("Expected IsKeyInUse('active_hit.mkv')=false after connection unregister")
	}
}

func TestS3ReadSeekerRespectsContextCancel(t *testing.T) {
	const bucket = "readseeker-cancel-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	backend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       bucket,
		Region:       "us-east-1",
		AccessKey:    "test-access",
		SecretKey:    "test-secret",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    16 * 1024,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	tempDir := t.TempDir()
	key := "ctx_cancel.mkv"
	data := bytes.Repeat([]byte("Z"), 64*1024)
	srcPath := filepath.Join(tempDir, key)
	if err := os.WriteFile(srcPath, data, 0644); err != nil {
		t.Fatal(err)
	}
	if err := backend.CommitLocalFile(context.Background(), key, srcPath, FileMeta{TotalSize: int64(len(data))}); err != nil {
		t.Fatalf("CommitLocalFile error: %v", err)
	}

	getStarted := make(chan struct{})
	var startedOnce sync.Once
	mockS3.SetRequestHook(func(r *http.Request) int {
		if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
			startedOnce.Do(func() { close(getStarted) })
			<-r.Context().Done()
			return 0
		}
		return 0
	})

	// 1. Canceling Open's context unblocks an in-flight stalled GetObject
	ctx, cancel := context.WithCancel(context.Background())
	rs, err := backend.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open error: %v", err)
	}
	defer rs.Close()

	errCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, rerr := rs.ReadAt(buf, 0)
		errCh <- rerr
	}()

	<-getStarted
	cancel()

	select {
	case rerr := <-errCh:
		if rerr == nil {
			t.Fatalf("Expected ReadAt error after context cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Timed out waiting for ReadAt to abort on canceled context")
	}

	// 2. Subsequent Read/ReadAt fail immediately even after buffered read-ahead succeeded
	mockS3.SetRequestHook(nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	rs2, err := backend.Open(ctx2, key)
	if err != nil {
		t.Fatalf("Open 2 error: %v", err)
	}
	defer rs2.Close()

	buf := make([]byte, 128)
	if n, err := rs2.Read(buf); err != nil || n != 128 {
		t.Fatalf("Initial Read error: n=%d err=%v", n, err)
	}
	cancel2()

	if _, err := rs2.Read(buf); !errors.Is(err, context.Canceled) {
		t.Errorf("Expected Read after cancel to return context.Canceled, got %v", err)
	}
	if _, err := rs2.ReadAt(buf, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Expected ReadAt after cancel to return context.Canceled, got %v", err)
	}
}
