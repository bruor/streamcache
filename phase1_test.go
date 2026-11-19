package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func saveAndRestorePhase1Globals(t *testing.T) {
	t.Helper()
	oldCfg := GlobalConfig
	oldReg := GlobalRegistry
	oldStorage := GlobalStorage
	oldS3Idx := GlobalS3Index
	oldPM := GlobalPrehydrationManager
	oldTopo := GetStorageTopology()

	t.Cleanup(func() {
		if GlobalS3Index != nil && GlobalS3Index != oldS3Idx {
			GlobalS3Index.Stop()
		}
		if GlobalRegistry != nil && GlobalRegistry != oldReg {
			GlobalRegistry.Stop()
		}
		if GlobalPrehydrationManager != nil && GlobalPrehydrationManager != oldPM {
			GlobalPrehydrationManager.Stop()
		}
		GlobalConfig = oldCfg
		GlobalRegistry = oldReg
		GlobalStorage = oldStorage
		GlobalS3Index = oldS3Idx
		GlobalPrehydrationManager = oldPM
		SetStorageTopology(oldTopo)
	})
}

func waitForPhase1Condition(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return cond()
}

func newPhase1SeekUpstream(data []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("ETag", `"phase1-etag"`)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.ServeContent(w, r, "video.mp4", time.Unix(1700000000, 0), bytes.NewReader(data))
	}))
}

func TestPhase1ConfigAndTopologyAutoDetection(t *testing.T) {
	saveAndRestorePhase1Globals(t)
	clearPhase0ConfigEnv(t)

	cfg := parseConfigFromEnv()
	if cfg.GetStateDir() != filepath.Join(cfg.GetTmpDir(), "state") {
		t.Fatalf("Expected default STATE_DIR=%q, got %q", filepath.Join(cfg.GetTmpDir(), "state"), cfg.GetStateDir())
	}
	if cfg.GetCacheMaxItemPct() != 50 {
		t.Fatalf("Expected default CACHE_MAX_ITEM_PCT=50, got %d", cfg.GetCacheMaxItemPct())
	}
	if cfg.GetNASSequentialStage() != "auto" {
		t.Fatalf("Expected default NAS_SEQUENTIAL_STAGE='auto', got %q", cfg.GetNASSequentialStage())
	}
	if cfg.EffectiveStorageBackend() != "disk" || !cfg.UsesCacheDir() {
		t.Fatalf("Expected effective='disk' & UsesCacheDir=true, got %q / %v", cfg.EffectiveStorageBackend(), cfg.UsesCacheDir())
	}

	// Explicit tmp & disabled CACHE_DIR variants
	for _, tc := range []struct {
		backend       string
		cacheDir      string
		wantEffective string
		wantUsesCache bool
	}{
		{"tmp", "/data/cache", "tmp", false},
		{"disk", "", "tmp", false},
		{"disk", "none", "tmp", false},
		{"disk", "OFF", "tmp", false},
		{"disk", "disabled", "tmp", false},
		{"disk", "/data/cache", "disk", true},
		{"s3", "/data/cache", "s3", false},
	} {
		t.Setenv("STORAGE_BACKEND", tc.backend)
		t.Setenv("CACHE_DIR", tc.cacheDir)
		c := parseConfigFromEnv()
		if c.EffectiveStorageBackend() != tc.wantEffective || c.UsesCacheDir() != tc.wantUsesCache {
			t.Errorf("backend=%q cacheDir=%q => got (%q, %v), want (%q, %v)",
				tc.backend, tc.cacheDir, c.EffectiveStorageBackend(), c.UsesCacheDir(), tc.wantEffective, tc.wantUsesCache)
		}
	}

	// Custom STATE_DIR, CACHE_MAX_ITEM_PCT bounds, NAS_SEQUENTIAL_STAGE validation
	clearPhase0ConfigEnv(t)
	t.Setenv("STATE_DIR", "/var/lib/streamcache")
	t.Setenv("CACHE_MAX_ITEM_PCT", "0")
	t.Setenv("NAS_SEQUENTIAL_STAGE", "bogus")
	cfg = parseConfigFromEnv()
	if cfg.GetStateDir() != "/var/lib/streamcache" {
		t.Errorf("Expected explicit STATE_DIR, got %q", cfg.GetStateDir())
	}
	if cfg.GetCacheMaxItemPct() != 0 {
		t.Errorf("Expected CACHE_MAX_ITEM_PCT=0 to disable %% cap, got %d", cfg.GetCacheMaxItemPct())
	}
	if warnings := strings.Join(detectLegacyConfigWarnings(), "\n"); !strings.Contains(warnings, "NAS_SEQUENTIAL_STAGE=bogus") {
		t.Errorf("Expected NAS_SEQUENTIAL_STAGE warning, got:\n%s", warnings)
	}
	t.Setenv("CACHE_MAX_ITEM_PCT", "150")
	if got := parseConfigFromEnv().GetCacheMaxItemPct(); got != 100 {
		t.Errorf("Expected CACHE_MAX_ITEM_PCT=150 clamped to 100, got %d", got)
	}

	// Synthetic /proc/self/mountinfo parsing & NAS sequential resolution
	mountInfo := "" +
		"25 1 0:20 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"44 25 0:42 / /mnt/nas rw,relatime - cifs //nas/media rw,vers=3.0\n" +
		"45 25 0:43 / /mnt/nas/my\\040media rw,relatime - fuse.rclone remote: rw\n" +
		"46 25 0:44 / /mnt/nfs rw,relatime - nfs4 10.0.0.5:/export rw\n"
	if got := parseMountInfoFSType(mountInfo, "/mnt/nas/files"); got != "cifs" {
		t.Errorf("Expected cifs for /mnt/nas/files, got %q", got)
	}
	if got := parseMountInfoFSType(mountInfo, "/mnt/nas/my media/movies"); got != "fuse.rclone" {
		t.Errorf("Expected longest prefix match fuse.rclone, got %q", got)
	}
	if got := parseMountInfoFSType(mountInfo, "/mnt/nfs/cache"); got != "nfs4" {
		t.Errorf("Expected nfs4, got %q", got)
	}
	if !resolveSequentialNASStage("auto", "cifs") || !resolveSequentialNASStage("auto", "smb3") || !resolveSequentialNASStage("auto", "fuse.rclone") {
		t.Errorf("Expected auto sequential stage true for cifs/smb3/fuse.*")
	}
	if resolveSequentialNASStage("auto", "ext4") || resolveSequentialNASStage("auto", "nfs4") {
		t.Errorf("Expected auto sequential stage false for ext4/nfs4")
	}
	if !resolveSequentialNASStage("true", "ext4") || resolveSequentialNASStage("false", "cifs") {
		t.Errorf("Expected explicit true/false NAS_SEQUENTIAL_STAGE overrides to win")
	}

	// Small-mount 25% clamp & shared-volume CACHE_MIN_DISK_FREE_BYTES floor
	tmpMin, cacheMin, clamped, raised := computeEffectiveLimits(
		1*1024*1024*1024, // configured 1GB
		512*1024*1024,  // 512MB TMP_FILE_MAX_BYTES
		256*1024*1024,  // configured 256MB CACHE_MIN_DISK_FREE_BYTES
		2*1024*1024*1024, // 2GB tmp mount => 25% = 512MB
		true, true,
	)
	if !clamped || tmpMin != 512*1024*1024 {
		t.Errorf("Expected small-mount clamp to 512MB, got clamped=%v tmpMin=%d", clamped, tmpMin)
	}
	if !raised || cacheMin != 1024*1024*1024 {
		t.Errorf("Expected shared-volume floor 1GB (512MB + 512MB), got raised=%v cacheMin=%d", raised, cacheMin)
	}
	// Separate volumes should NOT raise cacheMin
	_, cacheMinSep, _, raisedSep := computeEffectiveLimits(
		1*1024*1024*1024, 512*1024*1024, 256*1024*1024, 2*1024*1024*1024, true, false,
	)
	if raisedSep || cacheMinSep != 256*1024*1024 {
		t.Errorf("Expected separate volumes not to raise cacheMin, got raised=%v cacheMin=%d", raisedSep, cacheMinSep)
	}

	// Live InitStorageTopology on temp dirs skips CACHE_DIR creation when backend=tmp
	root := t.TempDir()
	tmpDir := filepath.Join(root, "tmp")
	stateDir := filepath.Join(root, "state")
	nonExistentCacheDir := filepath.Join(root, "should_not_exist_cache")
	tmpCfg := &Config{
		StorageBackend:        "tmp",
		TmpDir:                tmpDir,
		StateDir:              stateDir,
		CacheDir:              nonExistentCacheDir,
		TmpFileMaxBytes:       128 * 1024 * 1024,
		TmpMinDiskFreeBytes:   16 * 1024 * 1024,
		CacheMinDiskFreeBytes: 32 * 1024 * 1024,
	}
	topo := InitStorageTopology(tmpCfg)
	if topo == nil || topo.UsesCacheDir {
		t.Fatalf("Expected UsesCacheDir=false in tmp topology")
	}
	if FileExists(nonExistentCacheDir) {
		t.Fatalf("Expected InitStorageTopology not to create CACHE_DIR in tmp mode")
	}
	diskCacheDir := filepath.Join(root, "disk_cache")
	diskCfg := &Config{
		StorageBackend:        "disk",
		TmpDir:                tmpDir,
		StateDir:              stateDir,
		CacheDir:              diskCacheDir,
		TmpFileMaxBytes:       128 * 1024 * 1024,
		TmpMinDiskFreeBytes:   16 * 1024 * 1024,
		CacheMinDiskFreeBytes: 32 * 1024 * 1024,
	}
	diskTopo := InitStorageTopology(diskCfg)
	if diskTopo == nil || !diskTopo.SameFilesystem || diskTopo.CrossFSStaging {
		t.Fatalf("Expected temp dirs under same root to detect SameFilesystem=true, CrossFSStaging=false, got %+v", diskTopo)
	}
}

func TestPhase1ActiveFileSafeL1AdmissionAndBypass(t *testing.T) {
	saveAndRestorePhase1Globals(t)

	root := t.TempDir()
	cacheDir := filepath.Join(root, "cache")
	tmpDir := filepath.Join(root, "tmp")
	stateDir := filepath.Join(root, "state")

	cfg := &Config{
		StorageBackend:            "disk",
		CacheDir:                  cacheDir,
		TmpDir:                    tmpDir,
		StateDir:                  stateDir,
		CacheMaxBytes:             1000,
		CacheMaxItemPct:           50,
		CacheMinDiskFreeBytes:     0,
		TmpFileMaxBytes:           64 * 1024 * 1024,
		TmpMinDiskFreeBytes:       0,
		MinReadThresholdBytes:     1024,
		WritebackBufferSize:       16 * 1024 * 1024,
		MaxUpstreamWorkers:        1,
		MaxUpstreamRetries:        2,
		SchedulerMinTime:          10 * time.Millisecond,
		CacheRevalidateInterval:   1 * time.Hour,
		EnableEpisodePrehydration: false,
	}
	GlobalConfig = cfg
	GlobalPrehydrationManager = nil
	GlobalStorage = nil
	InitStorage()
	InitRegistry()

	// 1. >50% item rejected by CACHE_MAX_ITEM_PCT
	if admit, reason := GlobalRegistry.CanAdmitToDiskL1(501); admit {
		t.Fatalf("Expected 501 bytes (>50%% of 1000) to be rejected from L1 disk")
	} else if !strings.Contains(reason, "CACHE_MAX_ITEM_PCT") {
		t.Errorf("Expected CACHE_MAX_ITEM_PCT reason, got %q", reason)
	}
	if admit, reason := GlobalRegistry.CanAdmitToDiskL1(500); !admit {
		t.Fatalf("Expected 500 bytes (<=50%% of 1000) to be admitted, got rejected: %s", reason)
	}

	// 2. Disable item pct cap (0), pin 700 bytes active in CACHE_DIR + 200 bytes inactive
	cfg.SetCacheMaxItemPct(0)
	activeFile := filepath.Join(cacheDir, "active_playing.mp4")
	inactiveFile := filepath.Join(cacheDir, "inactive_old.mp4")
	if err := os.WriteFile(activeFile, bytes.Repeat([]byte("A"), 700), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inactiveFile, bytes.Repeat([]byte("I"), 200), 0644); err != nil {
		t.Fatal(err)
	}
	atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, 900)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Connections["c-active"] = &ClientConnection{
		ID:  "c-active",
		Key: "active_playing.mp4",
	}
	GlobalRegistry.Lock.Unlock()

	// 300 bytes fits (pinned 700 + 300 == 1000 <= 1000 after evicting inactive 200)
	if admit, reason := GlobalRegistry.CanAdmitToDiskL1(300); !admit {
		t.Fatalf("Expected 300 bytes to be admitted when pinned=700 & max=1000, got %s", reason)
	}
	// 400 bytes cannot fit without evicting active 700 (700 + 400 == 1100 > 1000)
	if admit, _ := GlobalRegistry.CanAdmitToDiskL1(400); admit {
		t.Fatalf("Expected 400 bytes to bypass L1 when pinned=700 & max=1000")
	}

	// 3. End-to-end HTTP stream of a bypassed oversized file in disk mode
	payload := make([]byte, 256*1024) // 256KB
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	upstream := newPhase1SeekUpstream(payload)
	defer upstream.Close()

	// Configure CACHE_MAX_BYTES = 300KB, CACHE_MAX_ITEM_PCT = 50 (max item 150KB),
	// keep active_playing.mp4 intact in CACHE_DIR.
	cfg.Lock()
	cfg.CacheMaxBytes = 300 * 1024
	cfg.CacheMaxItemPct = 50
	cfg.Unlock()
	SetStorageTopology(InitStorageTopology(cfg))

	proxySrv := httptest.NewServer(http.HandlerFunc(handleMediaStream))
	defer proxySrv.Close()

	streamURL := proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/oversized.mp4")
	resp, err := http.Get(streamURL)
	if err != nil {
		t.Fatalf("HTTP GET failed: %v", err)
	}
	got, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		t.Fatalf("ReadAll failed: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Expected 200 OK, got %d", resp.StatusCode)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("Streamed payload mismatch (got %d bytes, want %d bytes)", len(got), len(payload))
	}

	key, ext := getCacheKeyAndExt(upstream.URL + "/oversized.mp4")
	cacheKey := key + ext
	partPath := filepath.Join(tmpDir, cacheKey+".part")

	// Wait for post-close cleanup
	ok := waitForPhase1Condition(3*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, inReg := GlobalRegistry.Files[cacheKey]
		GlobalRegistry.Lock.RUnlock()
		return !inReg && !FileExists(partPath)
	})
	if !ok {
		t.Errorf("Expected bypassed TierTmp stream to remove registry entry and .part file after completion")
	}

	// CACHE_DIR must contain NO oversized file, NO .json for it, and active_playing.mp4 must be untouched.
	if FileExists(filepath.Join(cacheDir, cacheKey)) || FileExists(filepath.Join(cacheDir, cacheKey+".json")) {
		t.Errorf("Expected bypassed item not to be written into CACHE_DIR")
	}
	if !FileExists(activeFile) {
		t.Errorf("Expected existing active file in CACHE_DIR to remain untouched")
	}
}

func TestPhase1TmpAndS3ModesSkipCacheDirAndMetadata(t *testing.T) {
	saveAndRestorePhase1Globals(t)

	// --- Part A: Pure TMP mode + prehydration disabled + STATE_DIR preservation ---
	rootTmp := t.TempDir()
	tmpDir := filepath.Join(rootTmp, "tmp")
	stateDir := filepath.Join(tmpDir, "state") // intentionally nested inside TMP_DIR
	unusedCacheDir := filepath.Join(rootTmp, "cache_must_not_exist")

	cfgTmp := &Config{
		StorageBackend:            "tmp",
		CacheDir:                  unusedCacheDir,
		TmpDir:                    tmpDir,
		StateDir:                  stateDir,
		CacheMaxBytes:             512 * 1024 * 1024,
		CacheMaxItemPct:           50,
		TmpFileMaxBytes:           64 * 1024 * 1024,
		MinReadThresholdBytes:     1024,
		WritebackBufferSize:       16 * 1024 * 1024,
		MaxUpstreamWorkers:        1,
		MaxUpstreamRetries:        2,
		SchedulerMinTime:          10 * time.Millisecond,
		CacheRevalidateInterval:   1 * time.Hour,
		EnableEpisodePrehydration: true,
	}
	GlobalConfig = cfgTmp
	GlobalStorage = nil
	InitStorage()
	if GlobalStorage == nil || GlobalStorage.Type() != "tmp" {
		t.Fatalf("Expected TmpStorageBackend in tmp mode, got %v", GlobalStorage)
	}
	InitRegistry()
	if FileExists(unusedCacheDir) {
		t.Fatalf("Expected InitRegistry in tmp mode not to create CACHE_DIR")
	}
	if !FileExists(stateDir) {
		t.Fatalf("Expected STATE_DIR %s to be created and preserved across LoadExistingCache", stateDir)
	}

	// Seed marker in STATE_DIR (nested under TMP_DIR) and verify LoadExistingCache preserves subdirs
	markerFile := filepath.Join(stateDir, "keep_me.txt")
	if err := os.WriteFile(markerFile, []byte("ok"), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tmpDir, "stale.part"), []byte("123"), 0644)
	GlobalRegistry.LoadExistingCache()
	if !FileExists(markerFile) {
		t.Errorf("Expected LoadExistingCache to preserve nested STATE_DIR inside TMP_DIR")
	}
	if FileExists(filepath.Join(tmpDir, "stale.part")) {
		t.Errorf("Expected LoadExistingCache to remove stale .part file in TMP_DIR")
	}

	// Prehydration must be completely disabled in tmp mode
	pm := &PrehydrationManager{activePrehydrations: make(map[string]bool)}
	pm.latestCapacity = &ProviderCapacityReport{Status: "ok", TargetAvailableStreams: 10}
	if pm.CanStartPrehydration("http://example.com/ep1.mp4") {
		t.Errorf("Expected CanStartPrehydration=false in tmp mode")
	}
	pm.Enqueue("http://example.com/ep1.mp4", []string{"http://example.com/ep2.mp4"})
	if len(pm.queue) != 0 {
		t.Errorf("Expected Enqueue to be a no-op in tmp mode, got queue len %d", len(pm.queue))
	}

	// End-to-end HTTP stream in tmp mode
	payload := make([]byte, 128*1024)
	_, _ = rand.Read(payload)
	upstream := newPhase1SeekUpstream(payload)
	defer upstream.Close()

	proxySrv := httptest.NewServer(http.HandlerFunc(handleMediaStream))
	defer proxySrv.Close()

	resp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/ep1.mp4"))
	if err != nil {
		t.Fatalf("tmp mode GET error: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("tmp mode stream mismatch: status=%d len=%d", resp.StatusCode, len(got))
	}
	if FileExists(unusedCacheDir) {
		t.Errorf("Expected CACHE_DIR still not created after full stream in tmp mode")
	}

	key, ext := getCacheKeyAndExt(upstream.URL + "/ep1.mp4")
	tmpPart := filepath.Join(tmpDir, key+ext+".part")
	if !waitForPhase1Condition(3*time.Second, func() bool { return !FileExists(tmpPart) }) {
		t.Errorf("Expected %s to be deleted after tmp stream completion/close", tmpPart)
	}

	// --- Part B: S3 mode does NOT create CACHE_DIR, migrates legacy s3_index.json -> STATE_DIR ---
	rootS3 := t.TempDir()
	s3TmpDir := filepath.Join(rootS3, "tmp")
	s3StateDir := filepath.Join(rootS3, "state")
	legacyCacheDir := filepath.Join(rootS3, "legacy_cache")
	s3UnusedCacheDir := filepath.Join(rootS3, "unused_s3_cache")

	if err := os.MkdirAll(legacyCacheDir, 0755); err != nil {
		t.Fatal(err)
	}
	// Seed a valid legacy s3_index.json in legacyCacheDir and verify InitRegistry migrates it to STATE_DIR
	legacyIdx := NewS3AccessIndex(filepath.Join(legacyCacheDir, "s3_index.json"))
	legacyIdx.RecordCommit("migrated.mkv", "http://example.com/migrated.mkv", 42)
	if err := legacyIdx.Flush(); err != nil {
		t.Fatal(err)
	}
	if !FileExists(filepath.Join(legacyCacheDir, "s3_index.json")) {
		t.Fatalf("Expected legacy s3_index.json to be written before InitRegistry")
	}

	mockS3 := NewMockS3Server("test-bucket")
	defer mockS3.Close()

	GlobalConfig = &Config{
		StorageBackend: "s3",
		CacheDir:       legacyCacheDir,
		TmpDir:         s3TmpDir,
		StateDir:       s3StateDir,
		S3Config: S3Config{
			Endpoint:             mockS3.URL(),
			Bucket:               "test-bucket",
			Region:               "us-east-1",
			AccessKey:            "ak",
			SecretKey:            "sk",
			UsePathStyle:         true,
			DisableSSL:           true,
			ChunkSize:            64 * 1024,
			UploadConcurrency:    2,
			RecomposeConcurrency: 2,
		},
		TmpFileMaxBytes:         16 * 1024 * 1024,
		WritebackBufferSize:     16 * 1024 * 1024,
		CacheRevalidateInterval: 1 * time.Second,
	}
	GlobalStorage = nil
	InitStorage()
	InitRegistry()

	newIdxPath := filepath.Join(s3StateDir, "s3_index.json")
	if !FileExists(newIdxPath) {
		t.Fatalf("Expected s3_index.json to be migrated into STATE_DIR %s", newIdxPath)
	}
	count, _ := GlobalS3Index.Stats()
	if GlobalS3Index == nil || count != 1 {
		t.Errorf("Expected migrated GlobalS3Index to retain 1 entry, got count=%d (%+v)", count, GlobalS3Index)
	}

	// Verify S3 MaterializeDiskFile and Revalidate never create .json in CACHE_DIR
	GlobalConfig.Lock()
	GlobalConfig.CacheDir = s3UnusedCacheDir
	GlobalConfig.Unlock()
	GlobalConfig.SetStorageBackend("s3")
	SetStorageTopology(InitStorageTopology(GlobalConfig))
	if FileExists(s3UnusedCacheDir) {
		t.Fatalf("Expected S3 mode InitStorageTopology not to create CACHE_DIR %s", s3UnusedCacheDir)
	}

	s3Key := "s3_meta_test.mp4"
	if err := GlobalStorage.CommitLocalFile(context.Background(), s3Key, func() string {
		p := filepath.Join(s3TmpDir, "seed.part")
		_ = os.WriteFile(p, []byte("hello"), 0644)
		return p
	}(), FileMeta{
		URL:           upstream.URL + "/s3.mp4",
		TotalSize:     int64(len(payload)),
		ETag:          `"phase1-etag"`,
		LastValidated: time.Now().Add(-2 * time.Hour).Unix(),
		Completed:     true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := Revalidate(s3Key, upstream.URL+"/s3.mp4", nil); err != nil {
		t.Fatalf("S3 Revalidate error: %v", err)
	}
	s3State := NewFileCacheState(s3Key, upstream.URL+"/s3.mp4",
		filepath.Join(s3UnusedCacheDir, s3Key),
		filepath.Join(s3TmpDir, s3Key+".part"),
		filepath.Join(s3UnusedCacheDir, s3Key+".json"),
		"video/mp4", "", "", int64(len(payload)))
	if err := s3State.MaterializeDiskFile(); err != nil {
		t.Fatalf("MaterializeDiskFile error: %v", err)
	}
	if s3State.File != nil {
		_ = s3State.File.Close()
	}
	if FileExists(s3UnusedCacheDir) {
		t.Errorf("Expected neither Revalidate nor MaterializeDiskFile to create CACHE_DIR or .json files in S3 mode")
	}
}

type recordingChunkTarget struct {
	sequential bool
	mu         sync.Mutex
	order      []int
	chunks     map[int][]byte
	committed  atomic.Bool
}

func (r *recordingChunkTarget) Name() string       { return "mocknas" }
func (r *recordingChunkTarget) IsSequential() bool { return r.sequential }
func (r *recordingChunkTarget) Concurrency() int {
	if r.sequential {
		return 1
	}
	return 4
}
func (r *recordingChunkTarget) SupportsMultipart(totalSize, configured int64) (int64, bool) {
	return configured, true
}
func (r *recordingChunkTarget) PushChunk(ctx context.Context, key string, idx int, _ int64, rd io.Reader, size int64) error {
	b, err := io.ReadAll(rd)
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.chunks == nil {
		r.chunks = make(map[int][]byte)
	}
	r.order = append(r.order, idx)
	r.chunks[idx] = b
	r.mu.Unlock()
	return nil
}
func (r *recordingChunkTarget) Commit(ctx context.Context, key string, isMultipart bool, totalParts int, localPartPath string, meta FileMeta, onProgress CommitProgressFunc) error {
	r.committed.Store(true)
	return nil
}
func (r *recordingChunkTarget) ReadCommittedChunk(ctx context.Context, key string, idx int, _ int64, offsetInChunk, length int64) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.chunks[idx]
	if !ok {
		return nil, os.ErrNotExist
	}
	end := offsetInChunk + length
	if end > int64(len(b)) {
		end = int64(len(b))
	}
	return io.NopCloser(bytes.NewReader(b[offsetInChunk:end])), nil
}
func (r *recordingChunkTarget) Cleanup(ctx context.Context, key string) error { return nil }

func TestPhase1CrossFSNASStagingHolePunchAndSequentialOrder(t *testing.T) {
	saveAndRestorePhase1Globals(t)

	// 1. Test moveFileCrossFS EXDEV fallback via injectable renameFileFn
	exdevDir := t.TempDir()
	srcFile := filepath.Join(exdevDir, "src.bin")
	dstDir := filepath.Join(exdevDir, "dstdir")
	dstFile := filepath.Join(dstDir, "final.bin")
	sample := bytes.Repeat([]byte("EXDEV-FALLBACK-"), 4096)
	if err := os.WriteFile(srcFile, sample, 0644); err != nil {
		t.Fatal(err)
	}
	origRename := renameFileFn
	var renameCalls atomic.Int32
	renameFileFn = func(oldpath, newpath string) error {
		if renameCalls.Add(1) == 1 {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
		}
		return os.Rename(oldpath, newpath)
	}
	t.Cleanup(func() { renameFileFn = origRename })

	if err := moveFileCrossFS(srcFile, dstFile); err != nil {
		t.Fatalf("moveFileCrossFS EXDEV fallback failed: %v", err)
	}
	renameFileFn = origRename
	if FileExists(srcFile) || FileExists(dstFile+".staging") {
		t.Errorf("Expected srcFile and .staging to be removed after cross-FS move")
	}
	gotMoved, err := os.ReadFile(dstFile)
	if err != nil || !bytes.Equal(gotMoved, sample) {
		t.Fatalf("Cross-FS moved content mismatch (err=%v, len=%d)", err, len(gotMoved))
	}

	// 2. Test strict sequential chunk push order when IsSequential() == true
	seqDir := t.TempDir()
	chunkSize := int64(4096)
	totalSize := chunkSize * 3
	seqPart := filepath.Join(seqDir, "seq.part")
	partBytes := bytes.Repeat([]byte("S"), int(totalSize))
	if err := os.WriteFile(seqPart, partBytes, 0644); err != nil {
		t.Fatal(err)
	}
	seqState := &FileCacheState{
		Key:       "seq_test.mp4",
		TotalSize: totalSize,
		PartPath:  seqPart,
		RangeSet:  NewRangeSet(),
	}
	seqTarget := &recordingChunkTarget{sequential: true}
	cmSeq := NewScratchChunkManagerWithTarget(seqState, seqTarget, chunkSize)

	// Mark chunk 2 available BEFORE 0 and 1: must NOT push chunk 2 yet!
	seqState.RangeSet.Add(2*chunkSize, 3*chunkSize)
	cmSeq.CheckAndTriggerUploads()
	time.Sleep(50 * time.Millisecond)
	seqTarget.mu.Lock()
	if len(seqTarget.order) != 0 {
		t.Fatalf("Expected sequential target to wait for chunk 0 before pushing chunk 2, got %v", seqTarget.order)
	}
	seqTarget.mu.Unlock()

	// Make chunk 0 available, then chunk 1: must push 0 -> 1 -> 2 in strict order!
	seqState.RangeSet.Add(0, chunkSize)
	cmSeq.CheckAndTriggerUploads()
	if !waitForPhase1Condition(2*time.Second, func() bool {
		seqTarget.mu.Lock()
		defer seqTarget.mu.Unlock()
		return len(seqTarget.order) == 1
	}) {
		t.Fatalf("Timed out waiting for sequential chunk 0")
	}
	seqState.RangeSet.Add(chunkSize, 2*chunkSize)
	cmSeq.CheckAndTriggerUploads()
	if err := cmSeq.WaitAndRecompose(context.Background()); err != nil {
		t.Fatalf("WaitAndRecompose sequential failed: %v", err)
	}
	cmSeq.Close()

	seqTarget.mu.Lock()
	gotOrder := append([]int(nil), seqTarget.order...)
	seqTarget.mu.Unlock()
	if len(gotOrder) != 3 || gotOrder[0] != 0 || gotOrder[1] != 1 || gotOrder[2] != 2 {
		t.Errorf("Expected strict sequential chunk order [0 1 2], got %v", gotOrder)
	}
	if !seqTarget.committed.Load() {
		t.Errorf("Expected sequential target Commit() to run")
	}

	// Compare with parallel mode (IsSequential() == false): chunk 2 can push first when only 2 is ready
	parPart := filepath.Join(seqDir, "par.part")
	_ = os.WriteFile(parPart, partBytes, 0644)
	parState := &FileCacheState{
		Key:       "par_test.mp4",
		TotalSize: totalSize,
		PartPath:  parPart,
		RangeSet:  NewRangeSet(),
	}
	parTarget := &recordingChunkTarget{sequential: false}
	cmPar := NewScratchChunkManagerWithTarget(parState, parTarget, chunkSize)
	parState.RangeSet.Add(2*chunkSize, 3*chunkSize)
	cmPar.CheckAndTriggerUploads()
	if !waitForPhase1Condition(2*time.Second, func() bool {
		parTarget.mu.Lock()
		defer parTarget.mu.Unlock()
		return len(parTarget.order) == 1 && parTarget.order[0] == 2
	}) {
		t.Errorf("Expected non-sequential target to push ready chunk 2 immediately")
	}
	cmPar.Close()

	// 3. End-to-end HTTP stream + Range seek + cache HIT with simulated CrossFSStaging = true
	nasRoot := t.TempDir()
	nasTmpDir := filepath.Join(nasRoot, "tmp")
	nasCacheDir := filepath.Join(nasRoot, "nas_cache")
	nasStateDir := filepath.Join(nasRoot, "state")

	cfgNAS := &Config{
		StorageBackend:            "disk",
		CacheDir:                  nasCacheDir,
		TmpDir:                    nasTmpDir,
		StateDir:                  nasStateDir,
		CacheMaxBytes:             512 * 1024 * 1024,
		CacheMaxItemPct:           50,
		CacheMinDiskFreeBytes:     0,
		TmpFileMaxBytes:           128 * 1024, // 2 * 64KB chunk
		TmpMinDiskFreeBytes:       0,
		NASSequentialStage:        "true",
		MinReadThresholdBytes:     1024,
		WritebackBufferSize:       16 * 1024 * 1024,
		MaxUpstreamWorkers:        1,
		MaxUpstreamRetries:        2,
		SchedulerMinTime:          10 * time.Millisecond,
		CacheRevalidateInterval:   1 * time.Hour,
		EnableEpisodePrehydration: false,
		S3Config: S3Config{
			ChunkSize: 64 * 1024,
		},
	}
	GlobalConfig = cfgNAS
	GlobalPrehydrationManager = nil
	GlobalStorage = nil
	InitStorage()
	InitRegistry()
	SetStorageTopology(&StorageTopology{
		TmpDir:                nasTmpDir,
		CacheDir:              nasCacheDir,
		StateDir:              nasStateDir,
		UsesCacheDir:          true,
		SameFilesystem:        false,
		CrossFSStaging:        true,
		TmpFSType:             "ext4",
		CacheFSType:           "cifs",
		TmpPunchHoleSupported: probePunchHoleSupport(nasTmpDir),
		SequentialNASStage:    true,
	})

	payload := make([]byte, 256*1024) // 4 chunks * 64KB
	_, _ = rand.Read(payload)
	upstream := newPhase1SeekUpstream(payload)
	defer upstream.Close()

	proxySrv := httptest.NewServer(http.HandlerFunc(handleMediaStream))
	defer proxySrv.Close()

	// Live stream request
	resp, err := http.Get(proxySrv.URL + "/stream?url=" + url.QueryEscape(upstream.URL+"/nas_movie.mp4"))
	if err != nil {
		t.Fatalf("NAS HTTP GET failed: %v", err)
	}
	got, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil || resp.StatusCode != http.StatusOK || !bytes.Equal(got, payload) {
		t.Fatalf("NAS live stream mismatch: status=%d err=%v len=%d", resp.StatusCode, readErr, len(got))
	}

	nasKey, nasExt := getCacheKeyAndExt(upstream.URL + "/nas_movie.mp4")
	finalKey := nasKey + nasExt
	finalPath := filepath.Join(nasCacheDir, finalKey)
	stagingPath := finalPath + ".staging"
	partPath := filepath.Join(nasTmpDir, finalKey+".part")

	if !waitForPhase1Condition(5*time.Second, func() bool {
		GlobalRegistry.Lock.RLock()
		_, active := GlobalRegistry.Files[finalKey]
		GlobalRegistry.Lock.RUnlock()
		return !active && FileExists(finalPath) && FileExists(finalPath+".json") && !FileExists(stagingPath) && !FileExists(partPath)
	}) {
		t.Fatalf("Timed out waiting for cross-FS NAS staging commit (final=%v json=%v staging=%v part=%v)",
			FileExists(finalPath), FileExists(finalPath+".json"), FileExists(stagingPath), FileExists(partPath))
	}

	diskData, err := os.ReadFile(finalPath)
	if err != nil || !bytes.Equal(diskData, payload) {
		t.Fatalf("Committed NAS cache file content mismatch (err=%v, len=%d)", err, len(diskData))
	}

	// Range request against Cache HIT in NAS cache directory
	req, _ := http.NewRequest(http.MethodGet, proxySrv.URL+"/stream?url="+url.QueryEscape(upstream.URL+"/nas_movie.mp4"), nil)
	req.Header.Set("Range", "bytes=65536-131071")
	hitResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("NAS Range HIT request failed: %v", err)
	}
	hitBody, _ := io.ReadAll(hitResp.Body)
	_ = hitResp.Body.Close()
	if hitResp.StatusCode != http.StatusPartialContent || !bytes.Equal(hitBody, payload[65536:131072]) {
		t.Fatalf("NAS Range HIT mismatch: status=%d len=%d", hitResp.StatusCode, len(hitBody))
	}

	// 4. Test ReadCommittedChunk + CacheReader Tier 2b read while .staging is incomplete, and cancellation cleans up .staging
	cancelKey := "nas_cancel_test.mp4"
	cancelState := NewFileCacheState(cancelKey, "http://example.com/"+cancelKey,
		filepath.Join(nasCacheDir, cancelKey),
		filepath.Join(nasTmpDir, cancelKey+".part"),
		filepath.Join(nasCacheDir, cancelKey+".json"),
		"video/mp4", "", "", 192*1024)
	if cancelState.ChunkManager == nil || cancelState.ChunkManager.TargetName() != "nas" {
		t.Fatalf("Expected CrossFSStaging NewFileCacheState to attach NAS ChunkManager")
	}
	if err := os.WriteFile(cancelState.PartPath, payload[:192*1024], 0644); err != nil {
		t.Fatal(err)
	}
	cancelState.Lock.Lock()
	cancelState.DiskMaterialized = true
	cancelState.Lock.Unlock()
	// Upload ONLY chunk 0 (64KB) so .staging stays incomplete (not yet committed to finalPath)
	cancelState.RangeSet.Add(0, 64*1024)
	cancelState.ChunkManager.CheckAndTriggerUploads()
	if !waitForPhase1Condition(3*time.Second, func() bool {
		return cancelState.ChunkManager.IsChunkUploaded(0) && FileExists(filepath.Join(nasCacheDir, cancelKey+".staging"))
	}) {
		t.Fatalf("Expected chunk 0 to stage into .staging before cancel")
	}

	// Verify ReadCommittedChunk AND CacheReader.ReadAt Tier 2b serve exact bytes from .staging (not zeroes from hole-punched .part)
	rc, err := cancelState.ChunkManager.ReadCommittedChunk(context.Background(), 0, 123, 1024)
	if err != nil {
		t.Fatalf("ReadCommittedChunk error: %v", err)
	}
	stagedSlice, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(stagedSlice, payload[123:123+1024]) {
		t.Errorf("ReadCommittedChunk byte mismatch")
	}

	// Overwrite chunk 0 range in PartPath with zeroes to simulate confirmed hole-punch,
	// then verify CacheReader.ReadAt still returns original bytes via Tier 2b .staging!
	if pf, perr := os.OpenFile(cancelState.PartPath, os.O_RDWR, 0644); perr == nil {
		_, _ = pf.WriteAt(make([]byte, 64*1024), 0)
		_ = pf.Close()
	}
	cr := NewCacheReader(context.Background(), cancelState, nil)
	crBuf := make([]byte, 1024)
	crN, crErr := cr.ReadAt(crBuf, 123)
	if crErr != nil || crN != 1024 || !bytes.Equal(crBuf, payload[123:123+1024]) {
		t.Errorf("CacheReader Tier 2b staged read failed (got zeroes or err): n=%d err=%v", crN, crErr)
	}
	_ = cr.Close()

	// Verify CancelCoordinator removes both .part and .staging
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[cancelKey] = cancelState
	GlobalRegistry.Lock.Unlock()
	CancelCoordinator(cancelState)

	if !waitForPhase1Condition(3*time.Second, func() bool {
		return !FileExists(filepath.Join(nasCacheDir, cancelKey+".staging")) && !FileExists(cancelState.PartPath)
	}) {
		entries, _ := os.ReadDir(nasCacheDir)
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		t.Errorf("Expected CancelCoordinator to remove .staging and .part; cacheDir entries=%d", len(entries))
	}

	// Also verify errors.Is(syscall.EXDEV) sentinel unused import check safety
	if errors.Is(nil, syscall.EXDEV) {
		t.Fatal("unreachable")
	}
}
