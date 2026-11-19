package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func makeDeterministicPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte((i*131 + i/4096*17) & 0xFF)
	}
	return b
}

func TestPhase4RangeSetRemoveAndLiveBufferRemove(t *testing.T) {
	// 1. RangeSet.Remove edge cases
	rs := NewRangeSet()
	rs.Add(0, 100)

	// Disjoint remove
	if rem := rs.Remove(200, 300); rem != 0 {
		t.Fatalf("Expected disjoint Remove to return 0, got %d", rem)
	}

	// Splitting remove in the middle [25, 75)
	if rem := rs.Remove(25, 75); rem != 50 {
		t.Fatalf("Expected middle Remove to return 50, got %d", rem)
	}
	gotSlices := rs.ToSlices()
	if len(gotSlices) != 2 || gotSlices[0][0] != 0 || gotSlices[0][1] != 25 || gotSlices[1][0] != 75 || gotSlices[1][1] != 100 {
		t.Fatalf("Unexpected slices after middle split: %+v", gotSlices)
	}

	// Left trim [0, 10) and right trim [90, 120)
	if rem := rs.Remove(0, 10); rem != 10 {
		t.Fatalf("Expected left-trim Remove to return 10, got %d", rem)
	}
	if rem := rs.Remove(90, 120); rem != 10 {
		t.Fatalf("Expected right-trim Remove to return 10, got %d", rem)
	}
	if rs.TotalBytes() != 30 {
		t.Fatalf("Expected 30 total bytes remaining ([10,25) + [75,90)), got %d", rs.TotalBytes())
	}

	// Full wipe [0, 200)
	if rem := rs.Remove(0, 200); rem != 30 || rs.TotalBytes() != 0 {
		t.Fatalf("Expected full wipe to remove 30 bytes leaving 0, got rem=%d total=%d", rem, rs.TotalBytes())
	}

	// 2. LiveBuffer.RemoveRange skips pinned refCount > 1 chunks and drops refCount == 1 chunks.
	// Use >= 128KB offset spacing so PutSlice keeps separate pooledChunks instead of coalescing.
	lb := NewLiveBuffer(1024 * 1024)
	defer lb.Clear()

	const chunk1Off = 128 * 1024
	const chunk2Off = 256 * 1024
	lb.PutSlice(0, bytes.Repeat([]byte("A"), 1024), false)
	pinnedReq := lb.PutSlice(chunk1Off, bytes.Repeat([]byte("B"), 1024), true) // refCount == 2
	lb.PutSlice(chunk2Off, bytes.Repeat([]byte("C"), 1024), false)

	lb.RemoveRange(0, chunk2Off+512)

	buf := make([]byte, 1024)
	if _, ok := lb.ReadAt(buf, 0); ok {
		t.Errorf("Expected unpinned chunk 0 to be removed by LiveBuffer.RemoveRange")
	}
	if n, ok := lb.ReadAt(buf, chunk1Off); !ok || n != 1024 {
		t.Errorf("Expected pinned (refCount>1) chunk 1 to survive LiveBuffer.RemoveRange, ok=%v n=%d", ok, n)
	}
	if _, ok := lb.ReadAt(buf, chunk2Off); ok {
		t.Errorf("Expected overlapping unpinned chunk 2 to be removed by LiveBuffer.RemoveRange")
	}

	// Release write pin, verify RemoveRange can now drop chunk 1
	if pinnedReq.owner != nil {
		pinnedReq.owner.release()
	}
	lb.RemoveRange(chunk1Off, chunk1Off+1024)
	if _, ok := lb.ReadAt(buf, chunk1Off); ok {
		t.Errorf("Expected chunk 1 to be removed after write pin was released")
	}
}

func TestPhase4NonBlockingNASWatchdogDStateProtection(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	tmpDir := filepath.Join(base, "tmp")
	stateDir := filepath.Join(base, "state")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(tmpDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	payload := makeDeterministicPayload(256 * 1024)
	upstream := newPhase1SeekUpstream(payload)
	defer upstream.Close()

	GlobalConfig = &Config{
		StorageBackend:                "disk",
		CacheDir:                      cacheDir,
		TmpDir:                        tmpDir,
		StateDir:                      stateDir,
		CacheMaxBytes:                 10 * 1024 * 1024,
		CacheMaxItemPct:               50,
		TmpFileMaxBytes:               128 * 1024,
		TmpMinDiskFreeBytes:           0,
		CacheMinDiskFreeBytes:         0,
		WritebackBufferSize:           64 * 1024,
		MinReadThresholdBytes:         1024,
		MaxUpstreamWorkers:            1,
		MaxUpstreamRetries:            2,
		SchedulerMinTime:              10 * time.Millisecond,
		CacheRevalidateInterval:       1 * time.Hour,
		EnableEpisodePrehydration:     true,
		PrehydrateCapacityReservation: 1,
	}
	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		pollCh:              make(chan struct{}, 1),
	}
	GlobalStorage = nil
	if err := InitStorage(); err != nil {
		t.Fatalf("InitStorage error: %v", err)
	}
	InitRegistry()

	hm := getGlobalStorageHealth()
	if hm == nil {
		t.Fatalf("Expected GlobalStorageHealth to be initialized")
	}

	var probeCalls atomic.Int32
	var hang atomic.Bool
	hang.Store(true)
	hangCh := make(chan struct{})
	var hangOnce sync.Once
	releaseHang := func() {
		hang.Store(false)
		hangOnce.Do(func() { close(hangCh) })
	}
	defer releaseHang()

	hm.mu.Lock()
	hm.nasProbeTimeout = 80 * time.Millisecond
	hm.nasProbeFn = func(dir string) error {
		probeCalls.Add(1)
		if hang.Load() {
			<-hangCh
		}
		return nil
	}
	hm.mu.Unlock()

	start := time.Now()
	err := hm.ProbeNASOnce(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("Expected hung ProbeNASOnce to fail via bounded timeout")
	}
	if elapsed >= 500*time.Millisecond {
		t.Errorf("Expected D-state NAS timeout in <500ms, took %s", elapsed)
	}
	if IsNASHealthy() {
		t.Errorf("Expected IsNASHealthy()=false after D-state timeout")
	}
	if GlobalPrehydrationManager.CanStartPrehydration(upstream.URL + "/next.mp4") {
		t.Errorf("Expected CanStartPrehydration=false while only persistent tier (NAS) is offline")
	}

	// Second call while first probe is still blocked in D-state must NOT launch a second hung goroutine
	if err2 := hm.ProbeNASOnce(context.Background()); err2 == nil {
		t.Errorf("Expected second ProbeNASOnce while probe stuck to report error")
	}
	if got := probeCalls.Load(); got != 1 {
		t.Errorf("Expected no duplicate D-state probe goroutines while hung; probeCalls=%d", got)
	}

	proxySrv := httptest.NewServer(http.HandlerFunc(handleMediaStream))
	defer proxySrv.Close()

	// Playback during NAS outage must degrade immediately to TierTmp without stalling or touching CACHE_DIR
	ep1URL := upstream.URL + "/nas_down_ep1.mp4"
	resp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(ep1URL))
	if err != nil {
		t.Fatalf("HTTP GET during NAS outage failed: %v", err)
	}
	got1, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if rerr != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got1, payload) {
		t.Fatalf("Stream during NAS outage failed: status=%d len=%d err=%v", resp.StatusCode, len(got1), rerr)
	}

	k1, ext1 := getCacheKeyAndExt(ep1URL)
	ck1 := k1 + ext1
	waitForPhase1Condition(3*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, exists := GlobalRegistry.Files[ck1]
		GlobalRegistry.Lock.RUnlock()
		return !exists && !FileExists(filepath.Join(tmpDir, ck1+".part"))
	})
	if FileExists(filepath.Join(cacheDir, ck1)) || FileExists(filepath.Join(cacheDir, ck1+".json")) {
		t.Errorf("Expected NAS-outage stream not to write anything to CACHE_DIR")
	}

	// Unblock hung probe -> NAS recovers automatically
	releaseHang()
	if !waitForPhase1Condition(2*time.Second, IsNASHealthy) {
		t.Fatalf("Timed out waiting for NAS circuit breaker to close after D-state probe unblocked")
	}
	if !GlobalPrehydrationManager.CanStartPrehydration(upstream.URL + "/next.mp4") {
		t.Errorf("Expected CanStartPrehydration=true after NAS recovery")
	}

	// Subsequent stream after recovery caches normally to CACHE_DIR
	ep2URL := upstream.URL + "/nas_recovered_ep2.mp4"
	resp2, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(ep2URL))
	if err != nil {
		t.Fatalf("HTTP GET after recovery failed: %v", err)
	}
	got2, rerr2 := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if rerr2 != nil || resp2.StatusCode != http.StatusOK || !bytes.Equal(got2, payload) {
		t.Fatalf("Post-recovery stream failed: status=%d len=%d err=%v", resp2.StatusCode, len(got2), rerr2)
	}

	k2, ext2 := getCacheKeyAndExt(ep2URL)
	ck2 := k2 + ext2
	if !waitForPhase1Condition(3*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, inReg := GlobalRegistry.Files[ck2]
		GlobalRegistry.Lock.RUnlock()
		return !inReg && FileExists(filepath.Join(cacheDir, ck2))
	}) {
		t.Errorf("Expected ep2 to commit into CACHE_DIR and unregister after NAS recovery")
	}
}

func TestPhase4RingBufferOverTmpMaxForwardNoRefetchAndBackwardSeek(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	base := t.TempDir()
	tmpDir := filepath.Join(base, "tmp")
	stateDir := filepath.Join(base, "state")
	_ = os.MkdirAll(tmpDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	const totalSize = 512 * 1024 // 4x TMP_FILE_MAX_BYTES (128KB)
	payload := makeDeterministicPayload(totalSize)

	var reqMu sync.Mutex
	var reqStarts []int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := int64(0)
		end := int64(len(payload) - 1)
		if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
			parts := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)
			if s, err := strconv.ParseInt(parts[0], 10, 64); err == nil {
				start = s
			}
			if len(parts) > 1 && parts[1] != "" {
				if e, err := strconv.ParseInt(parts[1], 10, 64); err == nil && e < end {
					end = e
				}
			}
		}
		reqMu.Lock()
		reqStarts = append(reqStarts, start)
		reqMu.Unlock()

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	defer origin.Close()

	GlobalConfig = &Config{
		StorageBackend:          "tmp",
		TmpDir:                  tmpDir,
		StateDir:                stateDir,
		TmpFileMaxBytes:         128 * 1024,
		TmpMinDiskFreeBytes:     0,
		WritebackBufferSize:     64 * 1024,
		MinReadThresholdBytes:   1024,
		MaxUpstreamWorkers:      1,
		MaxUpstreamRetries:      2,
		SchedulerMinTime:        10 * time.Millisecond,
		CacheRevalidateInterval: 1 * time.Hour,
	}
	GlobalStorage = nil
	if err := InitStorage(); err != nil {
		t.Fatalf("InitStorage tmp error: %v", err)
	}
	InitRegistry()

	targetURL := origin.URL + "/ring_512k.mp4"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheKey := key + ext
	partPath := filepath.Join(tmpDir, cacheKey+".part")

	state := NewFileCacheState(cacheKey, targetURL, "", partPath, "", "video/mp4", "", "", totalSize)
	cm := state.GetChunkManager()
	if cm == nil || !cm.IsRingBufferMode() {
		t.Fatalf("Expected TierTmp state to attach RingBuffer ScratchChunkManager")
	}
	if cm.ChunkSize() != 32*1024 {
		t.Fatalf("Expected 32KB ring chunkSize for 128KB TMP_FILE_MAX_BYTES, got %d", cm.ChunkSize())
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[cacheKey] = state
	GlobalRegistry.Lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reader := NewCacheReader(ctx, state, nil)
	StartCoordinator(state, nil)

	// 1. Stream sequentially through entire 512KB (4x TMP_FILE_MAX_BYTES)
	gotAll := make([]byte, 0, totalSize)
	chunk := make([]byte, 16*1024)
	for off := int64(0); off < totalSize; {
		n, err := reader.ReadAt(chunk, off)
		if n > 0 {
			gotAll = append(gotAll, chunk[:n]...)
			off += int64(n)
		}
		if err != nil {
			t.Fatalf("Sequential ReadAt(%d) error: %v", off, err)
		}
	}
	if !bytes.Equal(gotAll, payload) {
		t.Fatalf("Ring-buffer forward stream byte mismatch (len got=%d want=%d)", len(gotAll), len(payload))
	}

	if cm.RingPunchedCount() <= 0 {
		t.Errorf("Expected ring-buffer punches > 0 when streaming 512KB through 128KB cap, got %d", cm.RingPunchedCount())
	}
	floorBeforeSeek := state.GetRingFloorOffset()
	if floorBeforeSeek <= 0 {
		t.Errorf("Expected ringFloorOffset > 0 after punching old chunks, got %d", floorBeforeSeek)
	}

	reqMu.Lock()
	zeroStartsForward := 0
	for _, s := range reqStarts {
		if s == 0 {
			zeroStartsForward++
		}
	}
	reqCountBeforeSeek := len(reqStarts)
	reqMu.Unlock()
	if zeroStartsForward != 1 {
		t.Errorf("Expected no unprompted forward re-fetches of punched offset 0; starts at 0=%d (all starts=%v)",
			zeroStartsForward, reqStarts)
	}

	// 2. Explicit backward seek behind ringFloorOffset must recover clean bytes from upstream
	seekBuf := make([]byte, 64*1024)
	sn, serr := io.ReadFull(io.NewSectionReader(reader, 0, int64(len(seekBuf))), seekBuf)
	if serr != nil || sn != len(seekBuf) {
		t.Fatalf("Backward seek ReadFull at offset 0 failed: n=%d err=%v", sn, serr)
	}
	if !bytes.Equal(seekBuf, payload[:64*1024]) {
		t.Fatalf("Backward seek returned corrupted/zeroed bytes instead of upstream data")
	}

	reqMu.Lock()
	reqCountAfterSeek := len(reqStarts)
	reqMu.Unlock()
	if reqCountAfterSeek <= reqCountBeforeSeek {
		t.Errorf("Expected backward seek to trigger upstream fetch for punched range")
	}

	_ = reader.Close()
	state.coordMu.Lock()
	state.coordMu.Unlock()
	if !waitForPhase1Condition(3*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, stillReg := GlobalRegistry.Files[cacheKey]
		GlobalRegistry.Lock.RUnlock()
		return !stillReg && !FileExists(partPath)
	}) {
		t.Errorf("Expected TierTmp ring-buffer .part file and registry entry to clean up after client close")
	}
}

func TestPhase4MidStreamS3OutageDegradesToRingBufferAndRecovers(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	const bucket = "midstream-s3-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	var injectMidStreamFail atomic.Bool
	injectMidStreamFail.Store(true)
	var failS3 atomic.Bool
	mockS3.RequestHook = func(r *http.Request) int {
		if failS3.Load() {
			return http.StatusServiceUnavailable
		}
		if injectMidStreamFail.Load() && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/chunks/") && !strings.HasSuffix(r.URL.Path, "/00000") {
			failS3.Store(true)
			return http.StatusServiceUnavailable
		}
		return 0
	}

	const totalSize = 384 * 1024 // 12 x 32KB chunks, 3x TMP_FILE_MAX_BYTES (128KB)
	payload := makeDeterministicPayload(totalSize)

	origin := newPhase1SeekUpstream(payload)
	defer origin.Close()

	base := t.TempDir()
	tmpDir := filepath.Join(base, "tmp")
	stateDir := filepath.Join(base, "state")
	_ = os.MkdirAll(tmpDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	GlobalConfig = &Config{
		StorageBackend: "s3",
		TmpDir:         tmpDir,
		StateDir:       stateDir,
		S3Config: S3Config{
			Endpoint:             mockS3.URL(),
			Bucket:               bucket,
			Region:               "us-east-1",
			AccessKey:            "test",
			SecretKey:            "test",
			UsePathStyle:         true,
			DisableSSL:           true,
			ChunkSize:            32 * 1024,
			UploadConcurrency:    1,
			RecomposeConcurrency: 2,
		},
		TmpFileMaxBytes:               128 * 1024,
		TmpMinDiskFreeBytes:           0,
		WritebackBufferSize:           64 * 1024,
		MinReadThresholdBytes:         1024,
		MaxUpstreamWorkers:            1,
		MaxUpstreamRetries:            2,
		SchedulerMinTime:              10 * time.Millisecond,
		CacheRevalidateInterval:       1 * time.Hour,
		EnableEpisodePrehydration:     true,
		PrehydrateCapacityReservation: 1,
	}
	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		pollCh:              make(chan struct{}, 1),
	}
	GlobalStorage = nil
	if err := InitStorage(); err != nil {
		t.Fatalf("InitStorage s3 error: %v", err)
	}
	InitRegistry()

	targetURL := origin.URL + "/s3_midstream.mp4"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheKey := key + ext
	partPath := filepath.Join(tmpDir, cacheKey+".part")

	state := NewFileCacheState(cacheKey, targetURL, "", partPath, "", "video/mp4", "", "", totalSize)
	cm := state.GetChunkManager()
	if cm == nil || cm.TargetName() != "s3" {
		t.Fatalf("Expected S3 ScratchChunkManager")
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[cacheKey] = state
	GlobalRegistry.Lock.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	// Start playback; chunk 00000 uploads cleanly and chunk 00001+ deterministically hits S3 503 outage
	reader := NewCacheReader(ctx, state, nil)
	StartCoordinator(state, nil)

	gotAll := make([]byte, 0, totalSize)
	chunk := make([]byte, 16*1024)
	for off := int64(0); off < totalSize; {
		n, err := reader.ReadAt(chunk, off)
		if n > 0 {
			gotAll = append(gotAll, chunk[:n]...)
			off += int64(n)
		}
		if err != nil {
			t.Fatalf("ReadAt(%d) during S3 outage failed: %v", off, err)
		}
	}
	if !bytes.Equal(gotAll, payload) {
		t.Fatalf("Mid-stream S3 failover payload mismatch")
	}
	if !waitForPhase1Condition(10*time.Second, func() bool {
		return cm.IsRingBufferMode() && !IsS3Healthy()
	}) {
		t.Fatalf("Expected ScratchChunkManager to enter ring-buffer mode (%v) and open S3 breaker (%v)",
			cm.IsRingBufferMode(), !IsS3Healthy())
	}
	if GlobalPrehydrationManager.CanStartPrehydration(origin.URL + "/next.mp4") {
		t.Errorf("Expected CanStartPrehydration=false while S3 is unhealthy in s3 mode")
	}

	// Backward seek to offset 0 (already punched earlier) while S3 is still 503 must recover from upstream!
	seekBuf := make([]byte, 32*1024)
	if _, err := io.ReadFull(io.NewSectionReader(reader, 0, int64(len(seekBuf))), seekBuf); err != nil {
		t.Fatalf("Backward seek to offset 0 while S3 offline failed: %v", err)
	}
	if !bytes.Equal(seekBuf, payload[:32*1024]) {
		t.Fatalf("Backward seek returned corrupted data")
	}

	_ = reader.Close()
	state.coordMu.Lock()
	state.coordMu.Unlock()
	waitForPhase1Condition(3*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, inReg := GlobalRegistry.Files[cacheKey]
		GlobalRegistry.Lock.RUnlock()
		return !inReg
	})

	// Clear S3 503 outage -> watchdog probe recovers and new stream commits cleanly to S3
	injectMidStreamFail.Store(false)
	failS3.Store(false)
	if err := getGlobalStorageHealth().ProbeS3Once(ctx); err != nil {
		t.Fatalf("ProbeS3Once after recovery error: %v", err)
	}
	if !IsS3Healthy() {
		t.Fatalf("Expected IsS3Healthy()=true after S3 recovery probe")
	}
	if !GlobalPrehydrationManager.CanStartPrehydration(origin.URL + "/next.mp4") {
		t.Errorf("Expected CanStartPrehydration=true after S3 recovery")
	}

	proxySrv := httptest.NewServer(http.HandlerFunc(handleMediaStream))
	defer proxySrv.Close()

	recURL := origin.URL + "/s3_recovered.mp4"
	resp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(recURL))
	if err != nil {
		t.Fatalf("Post-recovery S3 GET error: %v", err)
	}
	gotRec, rerr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if rerr != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(gotRec, payload) {
		t.Fatalf("Post-recovery S3 stream failed: status=%d len=%d err=%v", resp.StatusCode, len(gotRec), rerr)
	}

	rk, rext := getCacheKeyAndExt(recURL)
	recKey := rk + rext
	if !waitForPhase1Condition(10*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, inReg := GlobalRegistry.Files[recKey]
		GlobalRegistry.Lock.RUnlock()
		_, exists, _ := GlobalStorage.Stat(ctx, recKey)
		return !inReg && exists
	}) {
		t.Errorf("Expected post-recovery stream %s to commit cleanly to S3 and unregister", recKey)
	}
}

func TestPhase4HybridOutageRoutingMatrix(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	ctx := context.Background()
	const bucket = "hybrid-outage-bucket"
	mockS3 := NewMockS3Server(bucket)
	defer mockS3.Close()

	base := t.TempDir()
	cacheDir := filepath.Join(base, "cache")
	tmpDir := filepath.Join(base, "tmp")
	stateDir := filepath.Join(base, "state")
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(tmpDir, 0755)
	_ = os.MkdirAll(stateDir, 0755)

	GlobalConfig = &Config{
		StorageBackend:        "hybrid",
		CacheDir:              cacheDir,
		TmpDir:                tmpDir,
		StateDir:              stateDir,
		CacheMaxBytes:         4096,
		CacheMaxItemPct:       50,
		CacheMinDiskFreeBytes: 0,
		TmpFileMaxBytes:       128 * 1024,
		TmpMinDiskFreeBytes:   0,
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
		EnableEpisodePrehydration:     true,
		PrehydrateCapacityReservation: 1,
	}
	GlobalPrehydrationManager = &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		pollCh:              make(chan struct{}, 1),
	}

	// Seed an inactive local file (3000B of 4096B) so CanAdmitToHybridL1 rejects 1500B while CanAdmitToDiskL1 allows it
	if err := os.WriteFile(filepath.Join(cacheDir, "local_lru.mp4"), bytes.Repeat([]byte("L"), 3000), 0644); err != nil {
		t.Fatal(err)
	}

	GlobalStorage = nil
	if err := InitStorage(); err != nil {
		t.Fatalf("InitStorage hybrid error: %v", err)
	}
	InitRegistry()

	tiered := GlobalStorage.(*TieredStorageBackend)
	s3Src := filepath.Join(tmpDir, "s3_seed.src")
	_ = os.WriteFile(s3Src, bytes.Repeat([]byte("S"), 512), 0644)
	if err := tiered.S3().CommitLocalFile(ctx, "s3_cached.mp4", s3Src, FileMeta{
		URL:       "http://origin/s3_cached.mp4",
		TotalSize: 512,
		Completed: true,
	}); err != nil {
		t.Fatalf("seed S3 error: %v", err)
	}
	_ = os.Remove(s3Src)

	hm := getGlobalStorageHealth()
	demoter := getGlobalHybridDemoter()
	if hm == nil || demoter == nil {
		t.Fatalf("Expected StorageHealthMonitor and HybridDemoter in hybrid mode")
	}

	cleanupState := func(s *FileCacheState) {
		if cm := s.GetChunkManager(); cm != nil {
			cm.Close()
		}
	}

	// (a) NAS offline + S3 healthy -> route straight to TierS3, S3 HIT works, demoter pauses, prehydration enabled
	hm.RecordNASFailure(errors.New("EHOSTDOWN"))
	if IsNASHealthy() || !IsS3Healthy() {
		t.Fatalf("Expected NAS=false, S3=true")
	}
	if !GlobalPrehydrationManager.CanStartPrehydration("http://origin/next.mp4") {
		t.Errorf("Expected prehydration enabled when S3 is healthy despite NAS outage")
	}
	sA := NewFileCacheState("nas_down.mp4", "http://origin/nas_down.mp4", filepath.Join(cacheDir, "nas_down.mp4"), filepath.Join(tmpDir, "nas_down.part"), "", "video/mp4", "", "", 1000)
	cleanupState(sA)
	if sA.GetTargetTier() != TierS3 {
		t.Errorf("Expected hybrid with NAS offline to route 1000B stream to TierS3, got %s", sA.GetTargetTier())
	}
	if _, _, derr := demoter.RunDemotionPass(ctx); !errors.Is(derr, ErrStorageCircuitOpen) {
		t.Errorf("Expected RunDemotionPass to return ErrStorageCircuitOpen when NAS is offline, got %v", derr)
	}
	if meta, tier, exists, err := tiered.StatWithTier(ctx, "s3_cached.mp4"); err != nil || !exists || tier != TierS3 || meta == nil {
		t.Errorf("Expected S3 StatWithTier HIT while NAS offline; tier=%s exists=%v err=%v", tier, exists, err)
	}

	// (b) NAS recovered + S3 offline -> 1500B fits L1 with eviction (TierDisk), 3000B (>50% pct) falls back to TierTmp ring, demoter pauses
	hm.RecordNASSuccess()
	hm.RecordS3Failure(errors.New("503 service unavailable"))
	if !IsNASHealthy() || IsS3Healthy() {
		t.Fatalf("Expected NAS=true, S3=false")
	}
	if !GlobalPrehydrationManager.CanStartPrehydration("http://origin/next.mp4") {
		t.Errorf("Expected prehydration enabled when NAS is healthy despite S3 outage")
	}
	sB1 := NewFileCacheState("s3_down_fit.mp4", "http://origin/s3_down_fit.mp4", filepath.Join(cacheDir, "s3_down_fit.mp4"), filepath.Join(tmpDir, "s3_down_fit.part"), "", "video/mp4", "", "", 1500)
	cleanupState(sB1)
	if sB1.GetTargetTier() != TierDisk {
		t.Errorf("Expected hybrid with S3 offline and evictable L1 headroom to admit 1500B to TierDisk, got %s", sB1.GetTargetTier())
	}
	sB2 := NewFileCacheState("s3_down_oversize.mp4", "http://origin/s3_down_oversize.mp4", filepath.Join(cacheDir, "s3_down_oversize.mp4"), filepath.Join(tmpDir, "s3_down_oversize.part"), "", "video/mp4", "", "", 3000)
	cleanupState(sB2)
	if sB2.GetTargetTier() != TierTmp || sB2.GetChunkManager() == nil || !sB2.GetChunkManager().IsRingBufferMode() {
		t.Errorf("Expected oversized item while S3 offline to route to TierTmp ring buffer, got tier=%s", sB2.GetTargetTier())
	}
	if _, _, derr := demoter.RunDemotionPass(ctx); !errors.Is(derr, ErrStorageCircuitOpen) {
		t.Errorf("Expected RunDemotionPass to return ErrStorageCircuitOpen when S3 is offline, got %v", derr)
	}

	// (c) Both NAS and S3 offline -> TierTmp ring buffer + prehydration paused
	hm.RecordNASFailure(errors.New("ESTALE"))
	if HasHealthyPersistentTier(GlobalConfig, GlobalStorage) {
		t.Errorf("Expected HasHealthyPersistentTier=false when both NAS and S3 offline")
	}
	if GlobalPrehydrationManager.CanStartPrehydration("http://origin/next.mp4") {
		t.Errorf("Expected prehydration paused when both NAS and S3 are offline")
	}
	sC := NewFileCacheState("both_down.mp4", "http://origin/both_down.mp4", filepath.Join(cacheDir, "both_down.mp4"), filepath.Join(tmpDir, "both_down.part"), "", "video/mp4", "", "", 500)
	cleanupState(sC)
	if sC.GetTargetTier() != TierTmp || sC.GetChunkManager() == nil || !sC.GetChunkManager().IsRingBufferMode() {
		t.Errorf("Expected TierTmp ring buffer when both tiers offline, got tier=%s", sC.GetTargetTier())
	}
}
