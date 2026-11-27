package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPhase2ConfigHybridMode(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	t.Setenv("STORAGE_BACKEND", "hybrid")
	t.Setenv("CACHE_DIR", "/data/cache")
	cfg := parseConfigFromEnv()
	if cfg.EffectiveStorageBackend() != "hybrid" || !cfg.UsesCacheDir() {
		t.Fatalf("Expected effective='hybrid' & UsesCacheDir=true, got (%q, %v)",
			cfg.EffectiveStorageBackend(), cfg.UsesCacheDir())
	}
	if warnings := detectLegacyConfigWarnings(); len(warnings) != 0 {
		t.Errorf("Expected 0 config warnings for STORAGE_BACKEND=hybrid, got %v", warnings)
	}

	for _, disabledDir := range []string{"", "none", "OFF", "disabled"} {
		t.Setenv("STORAGE_BACKEND", "hybrid")
		t.Setenv("CACHE_DIR", disabledDir)
		c := parseConfigFromEnv()
		if c.EffectiveStorageBackend() != "s3" || c.UsesCacheDir() {
			t.Errorf("hybrid with CACHE_DIR=%q => got (%q, %v), want (\"s3\", false)",
				disabledDir, c.EffectiveStorageBackend(), c.UsesCacheDir())
		}
	}
}

func TestPhase2S3UploadFromFileMultipartAndAbort(t *testing.T) {
	mockS3 := NewMockS3Server("phase2-bucket")
	defer mockS3.Close()

	var uploadPartCalls atomic.Int32
	var chunkObjCalls atomic.Int32
	var putObjCalls atomic.Int32
	var failPart3 atomic.Bool

	mockS3.RequestHook = func(r *http.Request) int {
		if strings.Contains(r.URL.Path, "/chunks/") {
			chunkObjCalls.Add(1)
		}
		if r.Method == http.MethodPut {
			q := r.URL.Query()
			if q.Get("uploadId") != "" && q.Get("partNumber") != "" {
				uploadPartCalls.Add(1)
				if failPart3.Load() && q.Get("partNumber") == "3" {
					return http.StatusInternalServerError
				}
			} else if r.Header.Get("x-amz-copy-source") == "" {
				putObjCalls.Add(1)
			}
		}
		return 0
	}

	tmpDir := t.TempDir()
	chunkSize := int64(64 * 1024)
	s3Backend, err := NewS3StorageBackend(S3Config{
		Endpoint:             mockS3.URL(),
		Bucket:               "phase2-bucket",
		Region:               "us-east-1",
		AccessKey:            "ak",
		SecretKey:            "sk",
		UsePathStyle:         true,
		DisableSSL:           true,
		ChunkSize:            chunkSize,
		UploadConcurrency:    3,
		RecomposeConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("NewS3StorageBackend error: %v", err)
	}

	// 1. Multipart single-pass UploadFromFileWithProgress (5 parts * 64KB = 320KB)
	payload := make([]byte, 5*int(chunkSize))
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	srcPath := filepath.Join(tmpDir, "multi.bin")
	if err := os.WriteFile(srcPath, payload, 0644); err != nil {
		t.Fatal(err)
	}

	var stages []string
	var lastDone int
	err = s3Backend.UploadFromFileWithProgress(
		context.Background(),
		"multi.mp4",
		srcPath,
		FileMeta{URL: "http://example.com/multi.mp4", ETag: `"etag-multi"`},
		func(stage string, done, total int) {
			if len(stages) == 0 || stages[len(stages)-1] != stage {
				stages = append(stages, stage)
			}
			lastDone = done
		},
	)
	if err != nil {
		t.Fatalf("UploadFromFileWithProgress failed: %v", err)
	}
	if uploadPartCalls.Load() != 5 {
		t.Errorf("Expected 5 UploadPart calls, got %d", uploadPartCalls.Load())
	}
	if chunkObjCalls.Load() != 0 {
		t.Errorf("Expected 0 staging chunks/ calls during direct UploadFromFile, got %d", chunkObjCalls.Load())
	}
	if putObjCalls.Load() != 0 {
		t.Errorf("Expected 0 PutObject calls for multipart file, got %d", putObjCalls.Load())
	}
	if mockS3.ActiveUploads() != 0 {
		t.Errorf("Expected 0 dangling multipart uploads after completion, got %d", mockS3.ActiveUploads())
	}
	if len(stages) < 2 || stages[0] != "uploading" || stages[len(stages)-1] != "finalizing" || lastDone != 1 {
		t.Errorf("Unexpected progress transitions: stages=%v lastDone=%d", stages, lastDone)
	}

	rc, err := s3Backend.OpenRange(context.Background(), "multi.mp4", 0, 0)
	if err != nil {
		t.Fatalf("OpenRange multi.mp4 failed: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("Uploaded multipart payload mismatch (got %d bytes, want %d bytes)", len(got), len(payload))
	}
	meta, exists, err := s3Backend.Stat(context.Background(), "multi.mp4")
	if err != nil || !exists || meta.ETag != `"etag-multi"` || meta.TotalSize != int64(len(payload)) {
		t.Fatalf("Expected preserved metadata on vod/multi.mp4, got exists=%v err=%v meta=%+v", exists, err, meta)
	}

	// 2. Single-part fallback (<= 5MB under default 64MB chunkSize) + CommitLocalFile delegation
	putObjCalls.Store(0)
	uploadPartCalls.Store(0)
	smallBackend, err := NewS3StorageBackend(S3Config{
		Endpoint:     mockS3.URL(),
		Bucket:       "phase2-bucket",
		Region:       "us-east-1",
		AccessKey:    "ak",
		SecretKey:    "sk",
		UsePathStyle: true,
		DisableSSL:   true,
		ChunkSize:    64 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	smallData := bytes.Repeat([]byte("SINGLE"), 1024)
	smallPath := filepath.Join(tmpDir, "small.part")
	_ = os.WriteFile(smallPath, smallData, 0644)
	if err := smallBackend.CommitLocalFile(context.Background(), "small.mp4", smallPath, FileMeta{}); err != nil {
		t.Fatalf("CommitLocalFile single-part failed: %v", err)
	}
	if putObjCalls.Load() != 1 || uploadPartCalls.Load() != 0 {
		t.Errorf("Expected single-part PutObject=1 UploadPart=0, got PutObject=%d UploadPart=%d",
			putObjCalls.Load(), uploadPartCalls.Load())
	}

	// 3. Failure injection triggers AbortMultipartUpload leaving 0 active uploads
	failPart3.Store(true)
	if err := s3Backend.UploadFromFile(context.Background(), "abort_part3.mp4", srcPath, FileMeta{}); err == nil {
		t.Fatalf("Expected UploadFromFile error when part 3 fails")
	}
	if mockS3.ActiveUploads() != 0 {
		t.Errorf("Expected AbortMultipartUpload to clean up active upload, got %d remaining", mockS3.ActiveUploads())
	}
}

func TestPhase2TieredStorageBackendContractAndFallback(t *testing.T) {
	mockS3 := NewMockS3Server("tiered-bucket")
	defer mockS3.Close()

	var headCount atomic.Int32
	mockS3.RequestHook = func(r *http.Request) int {
		if r.Method == http.MethodHead {
			headCount.Add(1)
		}
		return 0
	}

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	tmpDir := filepath.Join(root, "tmp")

	local := NewLocalStorageBackend(cacheDir, tmpDir)
	s3Backend, err := NewS3StorageBackend(S3Config{
		Endpoint:             mockS3.URL(),
		Bucket:               "tiered-bucket",
		Region:               "us-east-1",
		AccessKey:            "ak",
		SecretKey:            "sk",
		UsePathStyle:         true,
		DisableSSL:           true,
		ChunkSize:            64 * 1024,
		UploadConcurrency:    2,
		RecomposeConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	tiered := NewTieredStorageBackend(local, s3Backend)

	// Full StorageBackend interface contract check
	runStorageContractTest(t, tiered, func(key string, data []byte) string {
		partPath := filepath.Join(tmpDir, key+".part")
		if err := os.WriteFile(partPath, data, 0644); err != nil {
			t.Fatal(err)
		}
		return partPath
	})

	ctx := context.Background()

	// Case A: Local L1 hit => 0 S3 HeadObject requests, TierDisk reader
	l1Data := bytes.Repeat([]byte("L1-LOCAL-"), 2048)
	l1Part := filepath.Join(tmpDir, "l1_only.part")
	_ = os.WriteFile(l1Part, l1Data, 0644)
	if err := tiered.CommitLocalFile(ctx, "l1_only.mp4", l1Part, FileMeta{ETag: `"l1"`}); err != nil {
		t.Fatal(err)
	}
	headCount.Store(0)
	metaL1, tierL1, exists, err := tiered.StatWithTier(ctx, "l1_only.mp4")
	if err != nil || !exists || tierL1 != TierDisk {
		t.Fatalf("Expected TierDisk StatWithTier hit, got tier=%q exists=%v err=%v", tierL1, exists, err)
	}
	rL1, err := tiered.OpenWithMeta(ctx, "l1_only.mp4", metaL1, tierL1)
	if err != nil {
		t.Fatalf("OpenWithMeta L1 failed: %v", err)
	}
	if resolveReaderTier(rL1, "") != TierDisk {
		t.Errorf("Expected TierDisk reader, got %q", resolveReaderTier(rL1, ""))
	}
	gotL1, _ := io.ReadAll(rL1)
	_ = rL1.Close()
	if !bytes.Equal(gotL1, l1Data) {
		t.Errorf("L1 content mismatch")
	}
	if headCount.Load() != 0 {
		t.Errorf("Expected 0 S3 HEAD calls for local L1 hit, got %d", headCount.Load())
	}

	// Case B: S3-only item => StatWithTier + OpenWithMeta issues ONLY 1 S3 HEAD total, 0 L1 files written
	s3Data := bytes.Repeat([]byte("S3-COLD-"), 2048)
	s3Part := filepath.Join(tmpDir, "s3_only.part")
	_ = os.WriteFile(s3Part, s3Data, 0644)
	if err := s3Backend.UploadFromFile(ctx, "s3_only.mp4", s3Part, FileMeta{ETag: `"s3"`}); err != nil {
		t.Fatal(err)
	}
	headCount.Store(0)
	metaS3, tierS3, exists, err := tiered.StatWithTier(ctx, "s3_only.mp4")
	if err != nil || !exists || tierS3 != TierS3 {
		t.Fatalf("Expected TierS3 StatWithTier hit, got tier=%q exists=%v err=%v", tierS3, exists, err)
	}
	rS3, err := tiered.OpenWithMeta(ctx, "s3_only.mp4", metaS3, tierS3)
	if err != nil {
		t.Fatalf("OpenWithMeta S3 failed: %v", err)
	}
	if resolveReaderTier(rS3, "") != TierS3 {
		t.Errorf("Expected TierS3 reader, got %q", resolveReaderTier(rS3, ""))
	}
	gotS3, _ := io.ReadAll(rS3)
	_ = rS3.Close()
	if !bytes.Equal(gotS3, s3Data) {
		t.Errorf("S3 fallback content mismatch")
	}
	if headCount.Load() != 1 {
		t.Errorf("Expected exactly 1 S3 HEAD call for StatWithTier + OpenWithMeta, got %d", headCount.Load())
	}
	if FileExists(filepath.Join(cacheDir, "s3_only.mp4")) {
		t.Errorf("Expected reading S3 fallback not to materialize file into CACHE_DIR")
	}

	// Case C: Item in both tiers prefers TierDisk, and Delete removes from BOTH
	_ = os.WriteFile(l1Part, l1Data, 0644)
	_ = tiered.CommitLocalFile(ctx, "both.mp4", l1Part, FileMeta{})
	_ = os.WriteFile(s3Part, s3Data, 0644)
	_ = s3Backend.UploadFromFile(ctx, "both.mp4", s3Part, FileMeta{})
	_, tierBoth, existsBoth, _ := tiered.StatWithTier(ctx, "both.mp4")
	if !existsBoth || tierBoth != TierDisk {
		t.Errorf("Expected both-tier item to prefer TierDisk, got exists=%v tier=%q", existsBoth, tierBoth)
	}
	if err := tiered.Delete(ctx, "both.mp4"); err != nil {
		t.Fatalf("Tiered Delete failed: %v", err)
	}
	if _, _, stillL1, _ := local.StatWithTier(ctx, "both.mp4"); stillL1 {
		t.Errorf("Expected Delete to remove both.mp4 from L1")
	}
	if _, _, stillS3, _ := s3Backend.StatWithTier(ctx, "both.mp4"); stillS3 {
		t.Errorf("Expected Delete to remove both.mp4 from S3")
	}
}

func TestPhase2HybridAdmissionBypassAndSingleStatHIT(t *testing.T) {
	saveAndRestorePhase1Globals(t)

	mockS3 := NewMockS3Server("hybrid-bucket")
	defer mockS3.Close()

	var headCount atomic.Int32
	mockS3.RequestHook = func(r *http.Request) int {
		if r.Method == http.MethodHead {
			headCount.Add(1)
		}
		return 0
	}

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	tmpDir := filepath.Join(root, "tmp")
	stateDir := filepath.Join(root, "state")

	cfg := &Config{
		StorageBackend: "hybrid",
		CacheDir:       cacheDir,
		TmpDir:         tmpDir,
		StateDir:       stateDir,
		CacheMaxBytes:  300 * 1024, // 300KB L1 ceiling
		CacheMaxItemPct: 50,        // 150KB max single L1 item
		CacheMinDiskFreeBytes: 0,
		TmpFileMaxBytes:       128 * 1024,
		TmpMinDiskFreeBytes:   0,
		MinReadThresholdBytes: 1024,
		WritebackBufferSize:   16 * 1024 * 1024,
		MaxUpstreamWorkers:    1,
		MaxUpstreamRetries:    2,
		SchedulerMinTime:      10 * time.Millisecond,
		CacheRevalidateInterval: 1 * time.Millisecond,
		EnableEpisodePrehydration: false,
		S3Config: S3Config{
			Endpoint:             mockS3.URL(),
			Bucket:               "hybrid-bucket",
			Region:               "us-east-1",
			AccessKey:            "ak",
			SecretKey:            "sk",
			UsePathStyle:         true,
			DisableSSL:           true,
			ChunkSize:            64 * 1024,
			UploadConcurrency:    2,
			RecomposeConcurrency: 2,
		},
	}
	GlobalConfig = cfg
	GlobalPrehydrationManager = nil
	GlobalStorage = nil
	if err := InitStorage(); err != nil {
		t.Fatalf("InitStorage hybrid error: %v", err)
	}
	InitRegistry()
	if GlobalStorage.Type() != "hybrid" {
		t.Fatalf("Expected hybrid storage backend, got %q", GlobalStorage.Type())
	}

	// Payloads:
	// - small: 120KB (<= 50% of 300KB = 150KB, fits L1 headroom) -> admitted to TierDisk
	// - oversized: 192KB (> 150KB = 50% cap) -> bypasses L1 directly to TierS3
	// - overflow: 120KB while L1 already has 200KB warm inactive items (200+120 > 300KB) -> bypasses to TierS3 without evicting L1
	smallData := make([]byte, 120*1024)
	oversizedData := make([]byte, 192*1024)
	overflowData := make([]byte, 120*1024)
	_, _ = rand.Read(smallData)
	_, _ = rand.Read(oversizedData)
	_, _ = rand.Read(overflowData)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		switch r.URL.Path {
		case "/small.mp4":
			body = smallData
		case "/oversized.mp4":
			body = oversizedData
		case "/overflow.mp4":
			body = overflowData
		default:
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "media.mp4", time.Unix(1700000000, 0), bytes.NewReader(body))
	}))
	defer upstream.Close()

	proxySrv := httptest.NewServer(http.HandlerFunc(handleMediaStream))
	defer proxySrv.Close()

	// 1. Small file admits to TierDisk L1 (NOT S3)
	resp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/small.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	gotSmall, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(gotSmall, smallData) {
		t.Fatalf("small.mp4 live stream mismatch: status=%d len=%d", resp.StatusCode, len(gotSmall))
	}

	smallKeyRaw, smallExt := getCacheKeyAndExt(upstream.URL + "/small.mp4")
	smallKey := smallKeyRaw + smallExt
	if !waitForPhase1Condition(3*time.Second, func() bool {
		return FileExists(filepath.Join(cacheDir, smallKey)) && FileExists(filepath.Join(cacheDir, smallKey+".json"))
	}) {
		t.Fatalf("Expected small.mp4 to commit to CACHE_DIR L1")
	}
	if mockS3.HasObject("hybrid-bucket", "vod/"+smallKey) {
		t.Errorf("Expected L1-admitted small.mp4 NOT to upload to S3 yet")
	}

	// Warm L1 HIT on small.mp4 => 0 S3 HEAD calls, active session CacheSource == "disk"
	headCount.Store(0)
	hitSmallResp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/small.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	oneByte := make([]byte, 1)
	if _, rerr := io.ReadFull(hitSmallResp.Body, oneByte); rerr != nil {
		t.Fatal(rerr)
	}
	var diskSourceSeen string
	for _, s := range GlobalRegistry.GetActiveSessions() {
		if s.Key == smallKey {
			diskSourceSeen = s.CacheSource
		}
	}
	restSmall, _ := io.ReadAll(hitSmallResp.Body)
	_ = hitSmallResp.Body.Close()
	if hitSmallResp.Header.Get("X-Cache") != "HIT" || !bytes.Equal(append(oneByte, restSmall...), smallData) {
		t.Fatalf("small.mp4 HIT mismatch: X-Cache=%q", hitSmallResp.Header.Get("X-Cache"))
	}
	if diskSourceSeen != string(TierDisk) {
		t.Errorf("Expected L1 HIT CacheSource='disk', got %q", diskSourceSeen)
	}
	if headCount.Load() != 0 {
		t.Errorf("Expected 0 S3 HEAD calls on warm L1 HIT, got %d", headCount.Load())
	}

	// 2. Oversized file bypasses L1 directly to TierS3 without touching CACHE_DIR
	overKeyRaw, overExt := getCacheKeyAndExt(upstream.URL + "/oversized.mp4")
	overKey := overKeyRaw + overExt

	resp2, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/oversized.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	gotOver, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK || !bytes.Equal(gotOver, oversizedData) {
		t.Fatalf("oversized.mp4 live stream mismatch: status=%d len=%d", resp2.StatusCode, len(gotOver))
	}

	if !waitForPhase1Condition(5*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, active := GlobalRegistry.Files[overKey]
		GlobalRegistry.Lock.RUnlock()
		return !active && mockS3.HasObject("hybrid-bucket", "vod/"+overKey)
	}) {
		t.Fatalf("Expected oversized.mp4 to commit to S3 vod/%s", overKey)
	}
	if FileExists(filepath.Join(cacheDir, overKey)) || FileExists(filepath.Join(cacheDir, overKey+".json")) {
		t.Errorf("Expected L1-bypassed oversized.mp4 NOT to create file or .json in CACHE_DIR")
	}
	if !FileExists(filepath.Join(cacheDir, smallKey)) {
		t.Errorf("Expected warm L1 small.mp4 to remain untouched when oversized.mp4 bypassed to S3")
	}
	if cnt, _ := GlobalS3Index.Stats(); cnt < 1 {
		t.Errorf("Expected GlobalS3Index to track bypassed S3 commit in hybrid mode, got count=%d", cnt)
	}

	// 3. Single-Stat S3 HIT verification (plus zero .json creation in CACHE_DIR on Revalidate)
	// Wait slightly > CacheRevalidateInterval (1ms) so RevalidateWithMeta also runs upstream probe!
	time.Sleep(5 * time.Millisecond)
	headCount.Store(0)

	hitS3Resp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/oversized.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if _, rerr := io.ReadFull(hitS3Resp.Body, oneByte); rerr != nil {
		t.Fatal(rerr)
	}
	var s3SourceSeen string
	for _, s := range GlobalRegistry.GetActiveSessions() {
		if s.Key == overKey {
			s3SourceSeen = s.CacheSource
		}
	}
	restOver, _ := io.ReadAll(hitS3Resp.Body)
	_ = hitS3Resp.Body.Close()

	if hitS3Resp.Header.Get("X-Cache") != "HIT" || !bytes.Equal(append(oneByte, restOver...), oversizedData) {
		t.Fatalf("oversized.mp4 S3 HIT mismatch: X-Cache=%q", hitS3Resp.Header.Get("X-Cache"))
	}
	if s3SourceSeen != string(TierS3) {
		t.Errorf("Expected S3 HIT CacheSource='s3', got %q", s3SourceSeen)
	}
	if headCount.Load() != 1 {
		t.Errorf("Expected EXACTLY 1 S3 HEAD request for cold S3 HIT (even with Revalidate), got %d", headCount.Load())
	}
	if FileExists(filepath.Join(cacheDir, overKey+".json")) {
		t.Errorf("Expected S3 HIT revalidation in hybrid mode NOT to write stray %s.json in CACHE_DIR", overKey)
	}

	// Also verify Range HIT on S3 item uses exactly 1 S3 HEAD
	headCount.Store(0)
	rangeReq, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/stream?url="+url.QueryEscape(upstream.URL+"/oversized.mp4"), nil)
	rangeReq.Header.Set("Range", "bytes=65536-131071")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	_ = rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || !bytes.Equal(rangeBody, oversizedData[65536:131072]) {
		t.Fatalf("S3 Range HIT mismatch: status=%d len=%d", rangeResp.StatusCode, len(rangeBody))
	}
	if headCount.Load() != 1 {
		t.Errorf("Expected exactly 1 S3 HEAD request for S3 Range HIT, got %d", headCount.Load())
	}

	// 4. Unallocated headroom overflow: seed extra 80KB warm file (120 + 80 = 200KB used out of 300KB),
	// then stream 120KB overflow.mp4 -> must bypass to TierS3 cleanly without synchronous eviction
	// (background HybridDemoter may asynchronously demote smallKey to S3 for headroom, preserving both).
	warmExtra := filepath.Join(cacheDir, "warm_extra.mp4")
	_ = os.WriteFile(warmExtra, bytes.Repeat([]byte("W"), 80*1024), 0644)

	overflowKeyRaw, overflowExt := getCacheKeyAndExt(upstream.URL + "/overflow.mp4")
	overflowKey := overflowKeyRaw + overflowExt
	resp3, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/overflow.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	gotOverflow, _ := io.ReadAll(resp3.Body)
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK || !bytes.Equal(gotOverflow, overflowData) {
		t.Fatalf("overflow.mp4 stream mismatch: status=%d len=%d", resp3.StatusCode, len(gotOverflow))
	}
	if !waitForPhase1Condition(5*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, active := GlobalRegistry.Files[overflowKey]
		GlobalRegistry.Lock.RUnlock()
		return !active && mockS3.HasObject("hybrid-bucket", "vod/"+overflowKey)
	}) {
		t.Fatalf("Expected unallocated-headroom overflow.mp4 to bypass to S3 vod/%s", overflowKey)
	}
	if !FileExists(warmExtra) {
		t.Errorf("Expected warm L1 file %s to remain on Disk", warmExtra)
	}
	if !FileExists(filepath.Join(cacheDir, smallKey)) && !mockS3.HasObject("hybrid-bucket", "vod/"+smallKey) {
		t.Errorf("Expected %s to remain warm on Disk or be safely demoted to S3 vod/%s", smallKey, smallKey)
	}
	if FileExists(filepath.Join(cacheDir, overflowKey)) {
		t.Errorf("Expected bypassed overflow.mp4 not to enter CACHE_DIR")
	}
}
