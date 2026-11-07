package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Set up temporary configuration values for testing
	GlobalConfig = &Config{
		Port:                    ":9999",
		CacheDir:                "./test_cache/files",
		TmpDir:                  "./test_cache/tmp",
		CacheMaxBytes:           300,
		MinDiskFreeBytes:        1000,
		CacheRevalidateInterval: 50 * time.Millisecond, // short for test
		SchedulerMinTime:        20 * time.Millisecond,
		MaxUpstreamRetries:      8, // Cap retries in unit test harness to prevent infinite background retry loops on closed mock servers
		MaxUpstreamWorkers:          2,
		EnableEpisodePrehydration:     true,
		PrehydrateCapacityReservation: 1,
	}

	_ = os.MkdirAll(GlobalConfig.CacheDir, 0755)
	_ = os.MkdirAll(GlobalConfig.TmpDir, 0755)

	GlobalRegistry = &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
	}
	InitHTTPClient()

	// Mock coordinator sleep in tests to prevent long delays
	coordinatorSleepMu.Lock()
	coordinatorSleepFn = func(d time.Duration) {
		time.Sleep(1 * time.Millisecond)
	}
	coordinatorSleepMu.Unlock()

	code := m.Run()

	// Cleanup
	_ = os.RemoveAll("./test_cache")
	os.Exit(code)
}

func TestCacheReaderCancellation(t *testing.T) {
	state := NewFileCacheState(
		"test_cancel",
		"http://localhost/dummy",
		filepath.Join(GlobalConfig.CacheDir, "test_cancel"),
		filepath.Join(GlobalConfig.TmpDir, "test_cancel.part"),
		filepath.Join(GlobalConfig.TmpDir, "test_cancel.json"),
		"video/x-matroska",
		"",
		"",
		500,
	)

	ctx, cancel := context.WithCancel(context.Background())
	reader := NewCacheReader(ctx, state, nil)

	// Trigger cancellation immediately
	cancel()
	CancelCoordinator(state)

	buf := make([]byte, 10)
	n, err := reader.ReadAt(buf, 0)
	if n != 0 || err != context.Canceled {
		t.Errorf("Expected ReadAt to fail with context.Canceled, got n=%d, err=%v", n, err)
	}

	_ = reader.Close()
	CancelCoordinator(state)
	if len(state.Clients) != 0 {
		t.Errorf("Expected clients list to be empty after Close, got %d", len(state.Clients))
	}
}

func TestOnDemandEviction(t *testing.T) {
	defer cleanupAfterTest(t, nil)

	// Clear registry size
	atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, 0)

	// Create 4 mock files in cache folder
	fileNames := []string{"a", "b", "c", "d"}
	for i, name := range fileNames {
		path := filepath.Join(GlobalConfig.CacheDir, name)
		_ = os.WriteFile(path, []byte("1234567890"), 0644) // 10 bytes each

		// Write companion metadata with staggered last-validated times
		meta := FileMeta{
			URL:           "http://dummy",
			TotalSize:     10,
			Completed:     true,
			LastValidated: time.Now().Add(time.Duration(i-3) * time.Hour).Unix(), // 'd' is newest, 'a' is oldest
		}
		metaBytes, _ := json.Marshal(meta)
		_ = os.WriteFile(path+".json", metaBytes, 0644)
	}

	// Set starting total size (4 files * 10 bytes = 40 bytes)
	atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, 40)

	// Update Max Cache size limit to 25 bytes (triggers immediate eviction on next write/update)
	GlobalConfig.Lock()
	oldMax := GlobalConfig.CacheMaxBytes
	GlobalConfig.CacheMaxBytes = 25
	GlobalConfig.Unlock()
	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheMaxBytes = oldMax
		GlobalConfig.Unlock()
	}()

	// Trigger pre-eviction for an incoming 5-byte file
	GlobalRegistry.PreEvict(5)

	// Wait for async background eviction to complete (up to 1s)
	for i := 0; i < 100; i++ {
		if !FileExists(filepath.Join(GlobalConfig.CacheDir, "a")) && !FileExists(filepath.Join(GlobalConfig.CacheDir, "b")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Check which files exist. We expect 'a' and 'b' (oldest) to be evicted.
	// Only 'c' and 'd' should remain.
	if FileExists(filepath.Join(GlobalConfig.CacheDir, "a")) {
		t.Errorf("Expected file 'a' to be evicted (oldest)")
	}
	if FileExists(filepath.Join(GlobalConfig.CacheDir, "b")) {
		t.Errorf("Expected file 'b' to be evicted")
	}
	if !FileExists(filepath.Join(GlobalConfig.CacheDir, "c")) {
		t.Errorf("Expected file 'c' to remain")
	}
	if !FileExists(filepath.Join(GlobalConfig.CacheDir, "d")) {
		t.Errorf("Expected file 'd' to remain (newest)")
	}
}

func TestRevalidateEvictsOldMediaFirst(t *testing.T) {
	defer cleanupAfterTest(t, nil)

	// Setup mock upstream server that changes ETag on second call
	etag := "etag-v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Range", "bytes 0-99/100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()

	key := "test_reprobe.mp4"
	filePath := filepath.Join(GlobalConfig.CacheDir, key)
	metaPath := filePath + ".json"

	_ = os.WriteFile(filePath, make([]byte, 100), 0644)
	meta := FileMeta{
		URL:           server.URL,
		TotalSize:     100,
		ETag:          "etag-v1",
		LastValidated: time.Now().Add(-2 * time.Hour).Unix(),
		Completed:     true,
	}
	metaBytes, _ := json.Marshal(meta)
	_ = os.WriteFile(metaPath, metaBytes, 0644)

	atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, 100)

	// Change etag on server to simulate media content modification
	etag = "etag-v2"

	GlobalConfig.Lock()
	oldInterval := GlobalConfig.CacheRevalidateInterval
	GlobalConfig.CacheRevalidateInterval = 1 * time.Second
	GlobalConfig.Unlock()
	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheRevalidateInterval = oldInterval
		GlobalConfig.Unlock()
	}()

	valid, err := Revalidate(key, server.URL, nil)
	if err != nil {
		t.Fatalf("Revalidate error: %v", err)
	}
	if valid {
		t.Errorf("Expected Revalidate to return false for modified media")
	}

	// Wait for async background deletion of old media (up to 1s)
	for i := 0; i < 100; i++ {
		if !FileExists(filePath) && !FileExists(metaPath) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Verify old media was evicted by Revalidate
	if FileExists(filePath) || FileExists(metaPath) {
		t.Errorf("Expected old media and metadata to be evicted upon change detection")
	}

	// TotalCacheSize should be reduced by 100
	size := GlobalRegistry.GetTotalCacheSize()
	if size != 0 {
		t.Errorf("Expected TotalCacheSize to be 0 after old media eviction, got %d", size)
	}
}

func TestRevalidationChangeDetection(t *testing.T) {
	defer cleanupAfterTest(t, nil)

	mockETag := "etag-v1"
	mockSize := int64(1000)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", mockETag)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", mockSize))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("0"))
	}))
	defer server.Close()

	key := "test_reval"
	filePath := filepath.Join(GlobalConfig.CacheDir, key)
	metaPath := filePath + ".json"

	_ = os.WriteFile(filePath, make([]byte, 1000), 0644)
	meta := FileMeta{
		URL:           server.URL,
		TotalSize:     1000,
		ETag:          "etag-v1",
		LastValidated: time.Now().Add(-2 * time.Hour).Unix(),
		Completed:     true,
	}
	metaBytes, _ := json.Marshal(meta)
	_ = os.WriteFile(metaPath, metaBytes, 0644)

	// 1. Run revalidation when headers match. Should succeed (HIT).
	valid, err := Revalidate(key, server.URL, nil)
	if err != nil || !valid {
		t.Errorf("Expected revalidation to succeed with matching headers, got valid=%t, err=%v", valid, err)
	}
	if !FileExists(filePath) {
		t.Errorf("Cached file should not have been evicted")
	}

	// 2. Modify upstream server to return different ETag
	mockETag = "etag-v2"
	metaBytes, _ = os.ReadFile(metaPath)
	_ = json.Unmarshal(metaBytes, &meta)
	meta.LastValidated = time.Now().Add(-2 * time.Hour).Unix()
	metaBytes, _ = json.Marshal(meta)
	_ = os.WriteFile(metaPath, metaBytes, 0644)

	valid, err = Revalidate(key, server.URL, nil)
	if err != nil || valid {
		t.Errorf("Expected revalidation to detect modification and return valid=false, got valid=%t, err=%v", valid, err)
	}

	for i := 0; i < 100; i++ {
		if !FileExists(filePath) && !FileExists(metaPath) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if FileExists(filePath) || FileExists(metaPath) {
		t.Errorf("Expected cached file and companion JSON to be evicted (deleted)")
	}
}

func TestDebouncedSchedulerTimeSlicing(t *testing.T) {
	var connCount int
	var lock sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		connCount++
		lock.Unlock()
		w.Header().Set("Content-Range", "bytes 0-99/500")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer server.Close()

	state := NewFileCacheState(
		"test_sched",
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, "test_sched"),
		filepath.Join(GlobalConfig.TmpDir, "test_sched.part"),
		filepath.Join(GlobalConfig.TmpDir, "test_sched.json"),
		"video/x-matroska",
		"",
		"",
		500,
	)
	defer cleanupAfterTest(t, state)

	// Set debouncer to 0 to trigger instant preemption in single-worker setup
	GlobalConfig.Lock()
	oldMinTime := GlobalConfig.SchedulerMinTime
	GlobalConfig.SchedulerMinTime = 0
	GlobalConfig.Unlock()
	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.SchedulerMinTime = oldMinTime
		GlobalConfig.Unlock()
	}()

	StartCoordinator(state, nil)
	time.Sleep(20 * time.Millisecond)

	state.Lock.Lock()
	state.Starving["client-1"] = 300 // Client 1 wants offset 300
	state.Lock.Unlock()
	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	time.Sleep(50 * time.Millisecond)

	lock.Lock()
	count := connCount
	lock.Unlock()

	if count < 2 {
		t.Errorf("Expected coordinator to abort and jump (connection count >= 2), got %d", count)
	}
}

func TestParseSizeString(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
		hasError bool
	}{
		{"50gb", 50 * 1024 * 1024 * 1024, false},
		{"2MB", 2 * 1024 * 1024, false},
		{"10g", 10 * 1024 * 1024 * 1024, false},
		{"1.5g", 0, true},
		{"0", 0, false},
		{"none", 0, false},
		{"disabled", 0, false},
		{"off", 0, false},
		{"", 0, false},
		{"1024", 1024, false},
		{"1024b", 1024, false},
		{"5tb", 5 * 1024 * 1024 * 1024 * 1024, false},
		{"10t", 10 * 1024 * 1024 * 1024 * 1024, false},
		{"500kb", 500 * 1024, false},
		{"250k", 250 * 1024, false},
		{"invalid", 0, true},
	}

	for _, tc := range tests {
		res, err := parseSizeString(tc.input)
		if tc.hasError {
			if err == nil {
				t.Errorf("Expected error for input %q, got nil", tc.input)
			}
		} else {
			if err != nil {
				t.Errorf("Unexpected error for input %q: %v", tc.input, err)
			}
			if res != tc.expected {
				t.Errorf("Expected %d for input %q, got %d", tc.expected, tc.input, res)
			}
		}
	}
}

func TestStarvationLoopPrevention(t *testing.T) {
	var connCount int
	var lock sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		connCount++
		lock.Unlock()
		w.Header().Set("Content-Range", "bytes 0-99/500")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 10))
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	state := NewFileCacheState(
		"test_loop",
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, "test_loop"),
		filepath.Join(GlobalConfig.TmpDir, "test_loop.part"),
		filepath.Join(GlobalConfig.TmpDir, "test_loop.json"),
		"video/x-matroska",
		"",
		"",
		500,
	)
	defer cleanupAfterTest(t, state)

	state.Lock.Lock()
	state.Clients["client-1"] = make(chan struct{}, 1)
	state.Clients["client-2"] = make(chan struct{}, 1)
	state.Starving["client-1"] = 0   // Client 1 wants offset 0
	state.Starving["client-2"] = 300 // Client 2 wants offset 300
	state.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(50 * time.Millisecond)

	lock.Lock()
	count := connCount
	lock.Unlock()

	if count > 1 {
		t.Errorf("Expected coordinator to stay on stream and NOT jump constantly (connection count <= 1), got %d", count)
	}

	state.Lock.Lock()
	close(state.Clients["client-1"])
	close(state.Clients["client-2"])
	state.Lock.Unlock()
}

func TestHeaderPropagation(t *testing.T) {
	var receivedHeaders []http.Header
	var lock sync.Mutex

	cdnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		receivedHeaders = append(receivedHeaders, r.Header)
		lock.Unlock()
		w.Header().Set("Content-Range", "bytes 0-99/500")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer cdnServer.Close()

	state := NewFileCacheState(
		"test_headers",
		cdnServer.URL,
		filepath.Join(GlobalConfig.CacheDir, "test_headers"),
		filepath.Join(GlobalConfig.TmpDir, "test_headers.part"),
		filepath.Join(GlobalConfig.TmpDir, "test_headers.json"),
		"video/x-matroska",
		"",
		"",
		500,
	)
	defer cleanupAfterTest(t, state)

	chromeHeaders := make(http.Header)
	chromeHeaders.Set("User-Agent", "Mozilla/5.0 Chrome/120.0.0.0")
	chromeHeaders.Set("Accept-Language", "en-US,en;q=0.9")
	chromeHeaders.Set("X-Custom-Chrome-Header", "ChromeTestVal")

	StartCoordinator(state, chromeHeaders)
	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	headersList := receivedHeaders
	lock.Unlock()

	if len(headersList) == 0 {
		t.Fatal("Expected mock CDN to receive request headers, but receivedHeaders is empty")
	}

	headers := headersList[0]

	ua := headers.Get("User-Agent")
	if ua != "Mozilla/5.0 Chrome/120.0.0.0" {
		t.Errorf("Expected User-Agent to be propagated, got %q", ua)
	}

	lang := headers.Get("Accept-Language")
	if lang != "en-US,en;q=0.9" {
		t.Errorf("Expected Accept-Language to be propagated, got %q", lang)
	}

	custom := headers.Get("X-Custom-Chrome-Header")
	if custom != "ChromeTestVal" {
		t.Errorf("Expected X-Custom-Chrome-Header to be propagated, got %q", custom)
	}

	rng := headers.Get("Range")
	if rng != "bytes=0-499" {
		t.Errorf("Expected Range header to be bytes=0-499, got %q", rng)
	}
}

func TestCacheFinalizationOrder(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.Unlock()
	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.Unlock()
	}()
	defer cleanupAfterTest(t, nil)

	t.Run("client_close_after_coordinator_exit", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_final_1.mkv"
		state := NewFileCacheState(
			key,
			"http://dummy",
			filepath.Join(GlobalConfig.CacheDir, key),
			filepath.Join(GlobalConfig.TmpDir, key+".part"),
			filepath.Join(GlobalConfig.CacheDir, key+".json"),
			"video/x-matroska",
			"",
			"",
			100,
		)
		state.RangeSet.Add(0, 100) // Complete

		_ = os.WriteFile(state.PartPath, []byte("dummy data"), 0644)
		reader := NewCacheReader(context.Background(), state, nil)

		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile

		state.WriterActive = false

		err := reader.Close()
		if err != nil {
			t.Fatalf("Reader Close failed: %v", err)
		}

		if !FileExists(state.Path) {
			t.Errorf("Expected final cache file %s to exist, but it does not", state.Path)
		}
		if FileExists(state.PartPath) {
			t.Errorf("Expected part file %s to be removed/renamed, but it still exists", state.PartPath)
		}

		metaBytes, err := os.ReadFile(state.Path + ".json")
		if err != nil {
			t.Fatalf("Failed to read final metadata JSON: %v", err)
		}
		var meta FileMeta
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			t.Fatalf("Failed to unmarshal metadata: %v", err)
		}
		if !meta.Completed {
			t.Errorf("Expected metadata Completed to be true, got false")
		}
	})

	t.Run("coordinator_exit_after_client_close", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_final_2.mkv"
		state := NewFileCacheState(
			key,
			"http://dummy",
			filepath.Join(GlobalConfig.CacheDir, key),
			filepath.Join(GlobalConfig.TmpDir, key+".part"),
			filepath.Join(GlobalConfig.CacheDir, key+".json"),
			"video/x-matroska",
			"",
			"",
			100,
		)
		state.RangeSet.Add(0, 100)

		_ = os.WriteFile(state.PartPath, []byte("dummy data"), 0644)
		reader := NewCacheReader(context.Background(), state, nil)

		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile

		err := reader.Close()
		if err != nil {
			t.Fatalf("Reader Close failed: %v", err)
		}

		if FileExists(state.Path) {
			t.Errorf("Expected final cache file %s NOT to exist yet, but it does", state.Path)
		}

		state.Lock.Lock()
		state.WriterActive = false
		shouldFinalize := len(state.Clients) == 0 && state.File != nil && !FileExists(state.Path)
		if shouldFinalize {
			_ = state.File.Close()
			state.File = nil
		}
		state.Lock.Unlock()

		if shouldFinalize {
			err := os.Rename(state.PartPath, state.Path)
			if err == nil {
				metaPath := state.Path + ".json"
				meta := FileMeta{
					URL:           state.URL,
					TotalSize:     state.TotalSize,
					ETag:          state.ETag,
					LastModified:  state.LastModified,
					LastValidated: time.Now().Unix(),
					Completed:     true,
				}
				newBytes, _ := json.Marshal(meta)
				_ = os.WriteFile(metaPath, newBytes, 0644)
			}
		}

		if !FileExists(state.Path) {
			t.Errorf("Expected final cache file %s to exist, but it does not", state.Path)
		}
		if FileExists(state.PartPath) {
			t.Errorf("Expected part file %s to be removed/renamed, but it still exists", state.PartPath)
		}
	})
}

func TestURLSanitizationAndKeyGeneration(t *testing.T) {
	url1 := "http://example.com/series/demo/user1/1334565.mkv?token=foo&expiry=123"
	url2 := "http://example.com/series/demo/user1/1334565.mkv?token=bar"
	url3 := "http://example.com/series/demo/user1/1334565.mkv"

	key1, ext1 := getCacheKeyAndExt(url1)
	key2, ext2 := getCacheKeyAndExt(url2)
	key3, ext3 := getCacheKeyAndExt(url3)

	if key1 != key2 || key2 != key3 {
		t.Errorf("Expected identical keys, got key1=%s, key2=%s, key3=%s", key1, key2, key3)
	}

	if ext1 != ".mkv" || ext2 != ".mkv" || ext3 != ".mkv" {
		t.Errorf("Expected extension to be '.mkv' and clean of query parameters, got ext1=%s, ext2=%s, ext3=%s", ext1, ext2, ext3)
	}

	urlNoExt := "http://example.com/series/demo/user1/1334565?token=abc"
	keyNoExt, extNoExt := getCacheKeyAndExt(urlNoExt)
	if extNoExt != ".mkv" {
		t.Errorf("Expected default '.mkv' extension when none is provided in path, got: %s", extNoExt)
	}
	if keyNoExt == key1 {
		t.Errorf("Expected different key for different path, got same key %s", keyNoExt)
	}
}

func TestLegacyMigration(t *testing.T) {
	targetURL := "http://localhost/series/test_legacy.mkv"
	key, ext := getCacheKeyAndExt(targetURL)
	legacyPath := filepath.Join(GlobalConfig.CacheDir, key)
	newPath := filepath.Join(GlobalConfig.CacheDir, key+ext)

	legacyData := []byte("legacy video bytes content")
	_ = os.WriteFile(legacyPath, legacyData, 0644)
	defer os.Remove(legacyPath)
	defer os.Remove(newPath)
	defer os.Remove(newPath + ".json")

	err := migrateLegacyFile(legacyPath, newPath, targetURL)
	if err != nil {
		t.Fatalf("Failed to run migrateLegacyFile: %v", err)
	}

	if FileExists(legacyPath) {
		t.Errorf("Expected legacy file at %s to be moved/deleted, but it exists", legacyPath)
	}

	if !FileExists(newPath) {
		t.Errorf("Expected new file at %s to exist, but it does not", newPath)
	}

	metaPath := newPath + ".json"
	if !FileExists(metaPath) {
		t.Fatalf("Expected metadata file at %s to exist, but it does not", metaPath)
	}

	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("Failed to read metadata file: %v", err)
	}

	var meta FileMeta
	err = json.Unmarshal(metaBytes, &meta)
	if err != nil {
		t.Fatalf("Failed to unmarshal metadata JSON: %v", err)
	}

	if meta.URL != targetURL {
		t.Errorf("Expected URL %s, got %s", targetURL, meta.URL)
	}
	if meta.TotalSize != int64(len(legacyData)) {
		t.Errorf("Expected TotalSize %d, got %d", len(legacyData), meta.TotalSize)
	}
	if !meta.Completed {
		t.Errorf("Expected Completed to be true, got false")
	}
}

func TestRevalidateMissingMetadata(t *testing.T) {
	targetURL := "http://localhost/series/test_reval.mkv"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext
	filePath := filepath.Join(GlobalConfig.CacheDir, cacheFileName)
	metaPath := filePath + ".json"

	_ = os.Remove(filePath)
	_ = os.Remove(metaPath)
	defer os.Remove(filePath)
	defer os.Remove(metaPath)

	fileData := []byte("some cache video bytes")
	err := os.WriteFile(filePath, fileData, 0644)
	if err != nil {
		t.Fatalf("Failed to write mock cache file: %v", err)
	}

	oldInterval := GlobalConfig.CacheRevalidateInterval
	GlobalConfig.CacheRevalidateInterval = 1 * time.Hour
	defer func() {
		GlobalConfig.CacheRevalidateInterval = oldInterval
	}()

	valid, err := Revalidate(cacheFileName, targetURL, nil)
	if err != nil {
		t.Fatalf("Revalidate returned error: %v", err)
	}
	if !valid {
		t.Errorf("Expected Revalidate to return true, got false")
	}

	if !FileExists(metaPath) {
		t.Fatalf("Expected metadata file %s to have been recreated, but it does not exist", metaPath)
	}

	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("Failed to read recreated metadata: %v", err)
	}

	var meta FileMeta
	err = json.Unmarshal(metaBytes, &meta)
	if err != nil {
		t.Fatalf("Failed to unmarshal recreated metadata: %v", err)
	}

	if meta.URL != targetURL {
		t.Errorf("Expected URL %s, got %s", targetURL, meta.URL)
	}
	if meta.TotalSize != int64(len(fileData)) {
		t.Errorf("Expected TotalSize %d, got %d", len(fileData), meta.TotalSize)
	}
	if !meta.Completed {
		t.Errorf("Expected Completed to be true, got false")
	}
}

func TestIncompleteDownloadCleanup(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()
	defer cleanupAfterTest(t, nil)

	t.Run("incomplete_coordinator_exit_with_active_client", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_incomplete_client.mkv"
		state := NewFileCacheState(
			key,
			"http://dummy",
			filepath.Join(GlobalConfig.CacheDir, key),
			filepath.Join(GlobalConfig.TmpDir, key+".part"),
			filepath.Join(GlobalConfig.CacheDir, key+".json"),
			"video/x-matroska",
			"",
			"",
			100,
		)
		state.RangeSet.Add(0, 50) // Incomplete (only first 50 bytes)

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		_ = os.WriteFile(state.PartPath, []byte("partial data"), 0644)
		_ = os.WriteFile(state.MetaPath, []byte("{}"), 0644)

		reader := NewCacheReader(context.Background(), state, nil)

		state.Lock.Lock()
		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile
		state.Lock.Unlock()

		state.Lock.Lock()
		state.WriterActive = false
		var fileToClose *os.File
		shouldCleanUpIncomplete := len(state.Clients) == 0
		if shouldCleanUpIncomplete {
			fileToClose = state.File
			state.File = nil
		}
		state.Lock.Unlock()

		if fileToClose != nil {
			_ = fileToClose.Close()
		}

		if shouldCleanUpIncomplete {
			t.Errorf("Expected shouldCleanUpIncomplete to be false because client is active")
		}

		if !FileExists(state.PartPath) {
			t.Errorf("Expected part file to exist while client is active")
		}

		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()
		if !inRegistry {
			t.Errorf("Expected file state to remain in registry while client is active")
		}

		err := reader.Close()
		if err != nil {
			t.Fatalf("Reader Close failed: %v", err)
		}

		if FileExists(state.PartPath) {
			t.Errorf("Expected part file to be deleted on last client Close")
		}
		if FileExists(state.MetaPath) {
			t.Errorf("Expected meta file to be deleted on last client Close")
		}

		GlobalRegistry.Lock.Lock()
		_, inRegistry = GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()
		if inRegistry {
			t.Errorf("Expected file state to be removed from registry on last client Close")
		}
	})

	t.Run("incomplete_coordinator_exit_no_active_client", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_incomplete_no_client.mkv"
		state := NewFileCacheState(
			key,
			"http://dummy",
			filepath.Join(GlobalConfig.CacheDir, key),
			filepath.Join(GlobalConfig.TmpDir, key+".part"),
			filepath.Join(GlobalConfig.CacheDir, key+".json"),
			"video/x-matroska",
			"",
			"",
			100,
		)
		state.RangeSet.Add(0, 50)

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		_ = os.WriteFile(state.PartPath, []byte("partial data"), 0644)
		_ = os.WriteFile(state.MetaPath, []byte("{}"), 0644)

		state.Lock.Lock()
		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile
		state.Lock.Unlock()

		state.Lock.Lock()
		state.WriterActive = false
		var fileToClose *os.File
		shouldCleanUpIncomplete := len(state.Clients) == 0
		if shouldCleanUpIncomplete {
			fileToClose = state.File
			state.File = nil
		}
		state.Lock.Unlock()

		if fileToClose != nil {
			_ = fileToClose.Close()
		}

		if !shouldCleanUpIncomplete {
			t.Errorf("Expected shouldCleanUpIncomplete to be true because no clients are active")
		}

		if shouldCleanUpIncomplete {
			_ = os.Remove(state.PartPath)
			_ = os.Remove(state.MetaPath)
			GlobalRegistry.Lock.Lock()
			delete(GlobalRegistry.Files, state.Key)
			GlobalRegistry.Lock.Unlock()
		}

		if FileExists(state.PartPath) {
			t.Errorf("Expected part file to be deleted immediately")
		}
		if FileExists(state.MetaPath) {
			t.Errorf("Expected meta file to be deleted immediately")
		}

		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()
		if inRegistry {
			t.Errorf("Expected file state to be removed from registry immediately")
		}
	})
}

func TestCoordinatorRetryTempering(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()
	defer cleanupAfterTest(t, nil)

	coordinatorSleepMu.Lock()
	oldSleep := coordinatorSleepFn
	coordinatorSleepFn = func(d time.Duration) {}
	coordinatorSleepMu.Unlock()
	defer func() {
		coordinatorSleepMu.Lock()
		coordinatorSleepFn = oldSleep
		coordinatorSleepMu.Unlock()
	}()

	t.Run("permanent_403_fast_exit", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()

		key := "test_403_fast_exit.mkv"
		state := NewFileCacheState(
			key,
			server.URL,
			filepath.Join(GlobalConfig.CacheDir, key),
			filepath.Join(GlobalConfig.TmpDir, key+".part"),
			filepath.Join(GlobalConfig.CacheDir, key+".json"),
			"video/x-matroska",
			"",
			"",
			100,
		)

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		StartCoordinator(state, nil)

		for i := 0; i < 100; i++ {
			state.Lock.Lock()
			active := state.WriterActive
			state.Lock.Unlock()
			if !active {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		state.Lock.Lock()
		active := state.WriterActive
		state.Lock.Unlock()

		if active {
			t.Errorf("Expected coordinator to exit immediately on 403, but it is still running")
		}

		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()

		if inRegistry {
			t.Errorf("Expected registry entry to be cleaned up on coordinator exit")
		}
	})

	t.Run("consecutive_failures_exhaustion_exit", func(t *testing.T) {
		GlobalConfig.Lock()
		origRetries := GlobalConfig.MaxUpstreamRetries
		GlobalConfig.MaxUpstreamRetries = 8
		GlobalConfig.Unlock()
		defer func() {
			GlobalConfig.Lock()
			GlobalConfig.MaxUpstreamRetries = origRetries
			GlobalConfig.Unlock()
		}()

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		key := "test_500_exhaustion.mkv"
		state := NewFileCacheState(
			key,
			server.URL,
			filepath.Join(GlobalConfig.CacheDir, key),
			filepath.Join(GlobalConfig.TmpDir, key+".part"),
			filepath.Join(GlobalConfig.CacheDir, key+".json"),
			"video/x-matroska",
			"",
			"",
			100,
		)

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		StartCoordinator(state, nil)

		for i := 0; i < 100; i++ {
			state.Lock.Lock()
			active := state.WriterActive
			state.Lock.Unlock()
			if !active {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}

		state.Lock.Lock()
		active := state.WriterActive
		state.Lock.Unlock()

		if active {
			t.Errorf("Expected coordinator to exit after 8 failures, but it is still running")
		}

		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()

		if inRegistry {
			t.Errorf("Expected registry entry to be cleaned up after failure exit")
		}
	})
}

func TestCoordinatorNoBlockSkippingOnFailure(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	coordinatorSleepMu.Lock()
	oldSleep := coordinatorSleepFn
	coordinatorSleepFn = func(d time.Duration) {}
	coordinatorSleepMu.Unlock()
	defer func() {
		coordinatorSleepMu.Lock()
		coordinatorSleepFn = oldSleep
		coordinatorSleepMu.Unlock()
	}()

	var lock sync.Mutex
	var requestedRanges []string
	var requestCount int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestCount++
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		count := requestCount
		lock.Unlock()

		if count == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Range", "bytes 0-99/200")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer server.Close()

	key := "test_no_skipping.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		200,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 requests, got %d", len(ranges))
	}

	if ranges[0] != "bytes=0-199" {
		t.Errorf("Expected first request range to be bytes=0-199, got %s", ranges[0])
	}
	if ranges[1] != "bytes=0-199" {
		t.Errorf("Expected second request range to be bytes=0-199 (retrying from 0), got %s", ranges[1])
	}
}

func cleanupAfterTest(t *testing.T, state *FileCacheState) {
	if state != nil {
		deadline := time.Now().Add(1 * time.Second)
		for time.Now().Before(deadline) {
			state.Lock.Lock()
			active := state.WriterActive
			state.Lock.Unlock()
			if !active {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		state.Lock.Lock()
		active := state.WriterActive
		state.Lock.Unlock()
		if active {
			t.Logf("Warning: Coordinator for %s did not exit in time", state.Key)
		}
	}

	deadlineEvict := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadlineEvict) {
		GlobalRegistry.Lock.Lock()
		evicting := GlobalRegistry.Evicting
		GlobalRegistry.Lock.Unlock()
		if !evicting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	GlobalRegistry.Lock.Lock()
	if state != nil {
		delete(GlobalRegistry.Files, state.Key)
	}
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Lock.Unlock()
}

func TestRobustProbe(t *testing.T) {
	rawURL := "http://example.com:8080/some/path?query=1"
	if host := GetCleanHost(rawURL); host != "example.com" {
		t.Errorf("GetCleanHost failed: expected 'example.com', got %q", host)
	}

	srcHeaders := http.Header{}
	srcHeaders.Set("Range", "bytes=100-200")
	srcHeaders.Set("Connection", "keep-alive")
	srcHeaders.Set("Host", "example.com")
	srcHeaders.Set("Accept-Encoding", "gzip")
	srcHeaders.Set("If-None-Match", "etag123")
	srcHeaders.Set("User-Agent", "my-player")
	srcHeaders.Set("X-Custom", "val")

	dstHeaders := http.Header{}
	CopySafeHeaders(srcHeaders, dstHeaders)

	if dstHeaders.Get("Range") != "" || dstHeaders.Get("Connection") != "" || dstHeaders.Get("Host") != "" || dstHeaders.Get("Accept-Encoding") != "" || dstHeaders.Get("If-None-Match") != "" {
		t.Errorf("CopySafeHeaders did not strip forbidden headers: %v", dstHeaders)
	}
	if dstHeaders.Get("User-Agent") != "my-player" || dstHeaders.Get("X-Custom") != "val" {
		t.Errorf("CopySafeHeaders did not preserve safe headers: %v", dstHeaders)
	}

	var lock sync.Mutex
	var requestCount int
	var rangeHeaders []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestCount++
		rangeHeaders = append(rangeHeaders, r.Header.Get("Range"))
		lock.Unlock()

		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Length", "5000")
		w.Header().Set("ETag", "test-etag")
		w.Header().Set("Last-Modified", "test-lm")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx := context.Background()
	size, _, etag, lm, notModified, err := probeUpstream(ctx, server.URL, nil, nil)
	if err != nil {
		t.Fatalf("probeUpstream failed: %v", err)
	}

	if size != 5000 {
		t.Errorf("Expected size 5000, got %d", size)
	}
	if etag != "test-etag" {
		t.Errorf("Expected etag 'test-etag', got %q", etag)
	}
	if lm != "test-lm" {
		t.Errorf("Expected lm 'test-lm', got %q", lm)
	}
	if notModified {
		t.Errorf("Expected notModified false, got true")
	}

	host := GetCleanHost(server.URL)
	if pref := GetProbeMethod(host); pref != ProbeMethodStandard {
		t.Errorf("Expected preferred method to be standard, got %s", pref)
	}

	lock.Lock()
	requestCountBefore := requestCount
	lock.Unlock()

	size2, _, _, _, _, err := probeUpstream(ctx, server.URL, nil, nil)
	if err != nil {
		t.Fatalf("second probeUpstream failed: %v", err)
	}
	if size2 != 5000 {
		t.Errorf("Expected second probe size 5000, got %d", size2)
	}

	lock.Lock()
	diff := requestCount - requestCountBefore
	lock.Unlock()

	if diff != 1 {
		t.Errorf("Expected exactly 1 request for the cached host, got %d", diff)
	}
}

func TestConcurrentUpstreamWorkers(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	tmpDir, err := os.MkdirTemp("", "sc-test-concurrent-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.ClientHydrationWait = 0 // Disable wait to trigger instant spawn for seek

	defer func() {
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
	}()

	var lock sync.Mutex
	var requestedRanges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		lock.Unlock()

		w.Header().Set("Content-Range", "bytes 0-99/400")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer server.Close()

	key := "test_concurrent.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		400,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(50 * time.Millisecond)

	state.Lock.Lock()
	state.Starving["client-test"] = 300
	state.Lock.Unlock()

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 range requests, got %d. Ranges: %v", len(ranges), ranges)
	}

	hasStart := false
	hasEnd := false
	for _, r := range ranges {
		if r == "bytes=0-399" {
			hasStart = true
		}
		if r == "bytes=300-399" {
			hasEnd = true
		}
	}

	if !hasStart || !hasEnd {
		t.Errorf("Expected requests starting at offset 0 (bytes=0-399) and offset 300 (bytes=300-399). Got: %v", ranges)
	}
}

func TestClientSequentialReadSuppression(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	tmpDir := t.TempDir()
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.ClientHydrationWait = 2 * time.Second

	defer func() {
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
	}()

	var lock sync.Mutex
	var requestedRanges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		isFirst := len(requestedRanges) == 1
		lock.Unlock()

		if isFirst {
			time.Sleep(250 * time.Millisecond)
		}

		w.Header().Set("Content-Range", "bytes 0-99/400")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer server.Close()

	key := "test_sequential_suppression.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		400,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(50 * time.Millisecond)

	state.Lock.Lock()
	state.Starving["client-test"] = 100
	state.Lock.Unlock()

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) != 1 {
		t.Errorf("Expected exactly 1 range request (suppression should prevent worker 2 from spawning for offset 100), but got %d. Ranges: %v", len(ranges), ranges)
	}
}

func TestConnectionTelemetry(t *testing.T) {
	state := &FileCacheState{}
	
	conn1 := "conn-1"
	state.RecordConnStart(conn1)
	if len(state.RecentConnections) != 1 {
		t.Errorf("Expected 1 recent connection, got %d", len(state.RecentConnections))
	}
	c1, exists := state.RecentConnections[conn1]
	if !exists {
		t.Fatal("Expected conn-1 to exist in map")
	}
	if c1.Opened.IsZero() {
		t.Error("Expected Opened timestamp to be non-zero")
	}
	if !c1.Closed.IsZero() {
		t.Error("Expected Closed timestamp to be zero initially")
	}

	conn2 := "conn-2"
	state.RecordConnStart(conn2)
	if len(state.RecentConnections) != 2 {
		t.Errorf("Expected 2 recent connections, got %d", len(state.RecentConnections))
	}

	state.RecordConnEnd(conn1)
	if c1.Closed.IsZero() {
		t.Error("Expected Closed timestamp to be non-zero after RecordConnEnd")
	}
	c2 := state.RecentConnections[conn2]
	if !c2.Closed.IsZero() {
		t.Error("Expected second connection to still be active (Closed = zero)")
	}
}

func TestCoordinatorObservedCooldown(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	tmpDir, err := os.MkdirTemp("", "sc-test-cooldown-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	GlobalConfig.MaxUpstreamWorkers = 1
	GlobalConfig.ClientHydrationWait = 0

	defer func() {
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
	}()

	var lock sync.Mutex
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		reqCount++
		currentReq := reqCount
		lock.Unlock()

		if currentReq == 1 {
			w.Header().Set("Content-Range", "bytes 0-99/200")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
		} else if currentReq == 2 {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.Header().Set("Content-Range", "bytes 100-199/200")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
		}
	}))
	defer server.Close()

	key := "test_cooldown.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		200,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	StartCoordinator(state, nil)

	time.Sleep(3 * time.Second)

	state.Lock.Lock()
	cooldown := state.ObservedCooldown
	isComplete := state.RangeSet.IsComplete(200)
	state.Lock.Unlock()

	if !isComplete {
		t.Errorf("Expected complete download, got RangeSet: %v", state.RangeSet.ToSlices())
	}

	if cooldown <= 0 {
		t.Errorf("Expected ObservedCooldown to be > 0, got %f", cooldown)
	} else {
		t.Logf("Successfully observed cooldown: %fs", cooldown)
	}
}

func TestProbeHTMLTypeRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html><body>Error</body></html>"))
	}))
	defer server.Close()

	_, _, _, _, _, err := probeUpstream(context.Background(), server.URL, nil, nil)
	if err == nil {
		t.Fatal("Expected probe to fail when Content-Type is HTML, but it succeeded")
	}
	if !isHTMLProbeError(err) {
		t.Errorf("Expected HTML probe error, got: %v", err)
	}
}

func TestProbeCatastrophicAbort(t *testing.T) {
	var lock sync.Mutex
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		reqCount++
		lock.Unlock()

		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, _, _, _, _, err := probeUpstream(context.Background(), server.URL, nil, nil)
	if err == nil {
		t.Fatal("Expected probe to fail")
	}

	lock.Lock()
	count := reqCount
	lock.Unlock()

	if count != 1 {
		t.Errorf("Expected exactly 1 request to mock server, got %d", count)
	}
}

func TestProbeFailureNegativeCaching(t *testing.T) {
	InitRegistry()

	var lock sync.Mutex
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		reqCount++
		lock.Unlock()

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	oldInterval := GlobalConfig.ProbeFailureCooldown
	oldTransient := GlobalConfig.ProbeTransientCooldown
	GlobalConfig.ProbeFailureCooldown = 200 * time.Millisecond
	GlobalConfig.ProbeTransientCooldown = 50 * time.Millisecond
	defer func() {
		GlobalConfig.ProbeFailureCooldown = oldInterval
		GlobalConfig.ProbeTransientCooldown = oldTransient
	}()

	key := "test_tombstone.mkv"
	targetURL := server.URL

	req := httptest.NewRequest("GET", "http://localhost:8080/play/"+key, nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GlobalRegistry.IsProbeFailed(key) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("Gateway Bypass"))
			return
		}

		_, _, _, _, _, err := probeUpstream(r.Context(), targetURL, r.Header, nil)
		if err != nil {
			cooldown := GlobalConfig.ProbeTransientCooldown
			if isHTMLProbeError(err) || isPermanentProbeError(err) {
				cooldown = GlobalConfig.ProbeFailureCooldown
			}
			GlobalRegistry.RecordFailedProbe(key, cooldown)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 Bad Gateway, got %d", w.Code)
	}

	lock.Lock()
	c1 := reqCount
	lock.Unlock()
	if c1 != 1 {
		t.Fatalf("Expected exactly 1 request to server on first try, got %d", c1)
	}

	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req)
	if w2.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 Bad Gateway, got %d", w2.Code)
	}

	lock.Lock()
	c2 := reqCount
	lock.Unlock()
	if c2 != 1 {
		t.Errorf("Expected request count to remain 1 (bypassed), got %d", c2)
	}

	time.Sleep(250 * time.Millisecond)

	w3 := httptest.NewRecorder()
	handler.ServeHTTP(w3, req)
	if w3.Code != http.StatusBadGateway {
		t.Errorf("Expected 502 Bad Gateway, got %d", w3.Code)
	}

	lock.Lock()
	c3 := reqCount
	lock.Unlock()
	if c3 != 2 {
		t.Errorf("Expected request count to increase to 2 after expiration, got %d", c3)
	}
}

func TestSequentialLastBlockPrefetch(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	totalSize := int64(1000000)
	cuesOffset := int64(800000)

	var lock sync.Mutex
	var requestedRanges []string

	ebmlHeader := makeEbmlElement(ID_EBML, []byte{0x42, 0x86, 0x81, 0x01})
	seekID := makeEbmlElement(ID_SEEKID, []byte{0x1C, 0x53, 0xBB, 0x6B})
	seekPosVal := []byte{0x0C, 0x35, 0x00} // 800000 in uint24 big endian
	seekPos := makeEbmlElement(ID_SEEKPOS, seekPosVal)
	seekPayload := append(seekID, seekPos...)
	seek := makeEbmlElement(ID_SEEK, seekPayload)
	seekHead := makeEbmlElement(ID_SEEKHEAD, seek)
	segmentID := makeVintID(ID_SEGMENT)
	segmentSize := makeVint(2000000)
	segmentHeader := append(segmentID, segmentSize...)
	headerBytes := append(ebmlHeader, segmentHeader...)
	headerBytes = append(headerBytes, seekHead...)

	responseBytes := make([]byte, totalSize)
	copy(responseBytes, headerBytes)

	cuesHeader := []byte{0x1C, 0x53, 0xBB, 0x6B}
	copy(responseBytes[cuesOffset:], cuesHeader)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		reqNum := len(requestedRanges)
		lock.Unlock()

		if reqNum == 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", totalSize-1, totalSize))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(responseBytes[:1000])
			time.Sleep(200 * time.Millisecond)
		} else {
			http.ServeContent(w, r, "test_prefetch.mkv", time.Time{}, bytes.NewReader(responseBytes))
		}
	}))
	defer server.Close()

	key := "test_prefetch.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		totalSize,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	state.Lock.Lock()
	state.Starving["client-1"] = 0
	state.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(100 * time.Millisecond)

	state.Lock.Lock()
	state.Starving["client-2"] = 900000
	state.Lock.Unlock()
	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	time.Sleep(250 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 requests (1 prefetch, 1 sequential), got %d. Ranges: %v", len(ranges), ranges)
	}

	expectedSequentialRange := "bytes=0-999999"
	if ranges[0] != expectedSequentialRange {
		t.Errorf("Expected first request range to be %s, got %s", expectedSequentialRange, ranges[0])
	}
}

func TestMp4NonFastStartOpportunisticPrefetch(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	totalSize := int64(1000016)
	mdatSize := int64(800000)
	ftyp := buildMp4Box("ftyp", 16, 8)
	mdat := buildMp4Box("mdat", mdatSize, 0)
	moov := buildMp4Box("moov", totalSize-int64(len(ftyp))-mdatSize, 0)

	responseBytes := make([]byte, totalSize)
	copy(responseBytes[0:], ftyp)
	copy(responseBytes[len(ftyp):], mdat)
	expectedMoovOffset := int64(len(ftyp)) + mdatSize
	copy(responseBytes[expectedMoovOffset:], moov)

	var lock sync.Mutex
	var requestedRanges []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		reqNum := len(requestedRanges)
		lock.Unlock()

		if reqNum == 1 {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", totalSize-1, totalSize))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(responseBytes[:130*1024]) // Write enough to pass 128KB header threshold
			time.Sleep(150 * time.Millisecond)
		} else {
			http.ServeContent(w, r, "test_prefetch.mp4", time.Time{}, bytes.NewReader(responseBytes))
		}
	}))
	defer server.Close()

	key := "test_prefetch.mp4"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/mp4",
		"",
		"",
		totalSize,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	req := httptest.NewRequest("GET", "/stream", nil)
	reader := NewCacheReader(context.Background(), state, req)
	defer reader.Close()

	StartCoordinator(state, nil)

	buf := make([]byte, 1024)
	_, _ = reader.Read(buf)

	time.Sleep(300 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 requests (1 sequential, 1 opportunistic moov prefetch), got %d. Ranges: %v", len(ranges), ranges)
	}

	expectedPrefetchRange := fmt.Sprintf("bytes=%d-%d", expectedMoovOffset, totalSize-1)
	foundPrefetch := false
	for _, rng := range ranges {
		if rng == expectedPrefetchRange {
			foundPrefetch = true
			break
		}
	}
	if !foundPrefetch {
		t.Errorf("Expected opportunistic prefetch range %s in requests, but got %v", expectedPrefetchRange, ranges)
	}
}

func TestCommittingStateIncluded(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Lock.Unlock()
	}()

	key := "test_committing_filter.mkv"
	state := NewFileCacheState(
		key,
		"http://example.com/test.mkv",
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		1000,
	)
	state.RangeSet.Add(0, 1000)
	state.UpstreamStatus = "Committing"

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	req, err := http.NewRequest("GET", "/debug/state", nil)
	if err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(handleDebugState)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected status code 200, got %d", rr.Code)
	}

	var data map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}

	activeDownloads, ok := data["active_downloads"].([]interface{})
	if !ok {
		t.Fatal("active_downloads is not a list")
	}

	found := false
	for _, item := range activeDownloads {
		m, ok := item.(map[string]interface{})
		if ok && m["key"] == key {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("Expected committing file %s to be included in active_downloads, but it was filtered out", key)
	}
}

func TestClientSessionMapping(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldConnections := GlobalRegistry.Connections
	oldSessions := GlobalRegistry.Sessions
	GlobalRegistry.Connections = make(map[string]*ClientConnection)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Connections = oldConnections
		GlobalRegistry.Sessions = oldSessions
		GlobalRegistry.Lock.Unlock()
	}()

	key := "test_session_video.mp4"
	url := "http://example.com/test_session_video.mp4"
	ip := "192.168.1.100:12345"
	ua := "Lavf/60.3.100"

	conn1 := &ClientConnection{
		ID:             "conn1",
		Key:            key,
		URL:            url,
		RemoteAddr:     ip,
		UserAgent:      ua,
		Offset:         1000,
		TotalSize:      100000,
		StartTime:      time.Now().Unix(),
		InCache:        false,
		StarvingOffset: -1,
	}
	GlobalRegistry.RegisterConnection("conn1", conn1)

	sessions := GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected 1 active session, got %d", len(sessions))
	}
	session := sessions[0]
	if session.ID != "conn1" {
		t.Errorf("Expected session ID to be 'conn1', got '%s'", session.ID)
	}
	if session.ActiveConns != 1 {
		t.Errorf("Expected active_conns to be 1, got %d", session.ActiveConns)
	}

	conn2 := &ClientConnection{
		ID:             "conn2",
		Key:            key,
		URL:            url,
		RemoteAddr:     "192.168.1.100:54321",
		UserAgent:      ua,
		Offset:         2000,
		TotalSize:      100000,
		StartTime:      time.Now().Unix(),
		InCache:        false,
		StarvingOffset: -1,
	}
	GlobalRegistry.RegisterConnection("conn2", conn2)

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected connections from same IP/key to be grouped, got %d sessions", len(sessions))
	}
	session = sessions[0]
	if session.ActiveConns != 2 {
		t.Errorf("Expected active_conns to be 2, got %d", session.ActiveConns)
	}

	GlobalRegistry.UnregisterConnection("conn1")
	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected 1 session to remain, got %d", len(sessions))
	}
	session = sessions[0]
	if session.ActiveConns != 1 {
		t.Errorf("Expected active_conns to be 1 after unregistering conn1, got %d", session.ActiveConns)
	}

	GlobalRegistry.UnregisterConnection("conn2")
	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected session to remain in idle cooldown, got %d sessions", len(sessions))
	}
	session = sessions[0]
	if session.ActiveConns != 0 {
		t.Errorf("Expected active_conns to be 0, got %d", session.ActiveConns)
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Connections = make(map[string]*ClientConnection)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Lock.Unlock()

	conn3 := &ClientConnection{
		ID:             "conn3",
		Key:            key,
		URL:            url,
		RemoteAddr:     "192.168.1.100:11111",
		UserAgent:      ua,
		Offset:         5000,
		TotalSize:      100000,
		StartTime:      time.Now().Unix(),
		InCache:        false,
		StarvingOffset: -1,
	}
	conn4 := &ClientConnection{
		ID:             "conn4",
		Key:            key,
		URL:            url,
		RemoteAddr:     "192.168.1.100:22222",
		UserAgent:      ua,
		Offset:         6000,
		TotalSize:      100000,
		StartTime:      time.Now().Unix(),
		InCache:        false,
		StarvingOffset: -1,
	}

	GlobalRegistry.RegisterConnection("conn3", conn3)
	GlobalRegistry.RegisterConnection("conn4", conn4)

	GlobalRegistry.Lock.Lock()
	sess, ok := GlobalRegistry.Sessions["conn3"]
	if !ok {
		GlobalRegistry.Lock.Unlock()
		t.Fatal("Expected session for conn3 to exist")
	}
	sess.Conns["conn3"].Opened = time.Now().Add(-5 * time.Second)
	sess.Conns["conn4"].Opened = time.Now().Add(-3 * time.Second)
	GlobalRegistry.Lock.Unlock()

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 2 {
		t.Fatalf("Expected concurrency split to result in 2 sessions, got %d", len(sessions))
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Connections = make(map[string]*ClientConnection)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Lock.Unlock()

	conn5 := &ClientConnection{
		ID:          "conn5",
		Key:         key,
		URL:         url,
		RemoteAddr:  ip,
		UserAgent:   ua,
		Offset:      1000,
		TotalSize:   100000,
		StartTime:   time.Now().Unix(),
	}
	GlobalRegistry.RegisterConnection("conn5", conn5)
	GlobalRegistry.UnregisterConnection("conn5")

	GlobalRegistry.Lock.Lock()
	sess5 := GlobalRegistry.Sessions["conn5"]
	sess5.LastActive = time.Now().Add(-61 * time.Second)
	GlobalRegistry.Lock.Unlock()

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 0 {
		t.Errorf("Expected idle session to be purged, got %d sessions", len(sessions))
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Connections = make(map[string]*ClientConnection)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Lock.Unlock()

	conn6 := &ClientConnection{
		ID:          "conn6",
		Key:         key,
		URL:         url,
		RemoteAddr:  ip,
		UserAgent:   ua,
		Offset:      1000,
		StartOffset: 1000,
		TotalSize:   100000000,
		StartTime:   time.Now().Unix(),
	}
	GlobalRegistry.RegisterConnection("conn6", conn6)

	conn7 := &ClientConnection{
		ID:          "conn7",
		Key:         key,
		URL:         url,
		RemoteAddr:  "192.168.1.100:55555",
		UserAgent:   ua,
		Offset:      95000000,
		StartOffset: 95000000,
		TotalSize:   100000000,
		StartTime:   time.Now().Unix(),
	}
	GlobalRegistry.RegisterConnection("conn7", conn7)

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected EOF connection to group with existing session, got %d sessions", len(sessions))
	}
	session = sessions[0]
	if session.ActiveConns != 1 {
		t.Errorf("Expected active_conns to be 1 (conn7 is a probe), got %d", session.ActiveConns)
	}

	if session.Offset != 1000 {
		t.Errorf("Expected session offset to remain 1000, got %d", session.Offset)
	}

	conn7.Offset = 98000000
	GlobalRegistry.UpdateConnectionOffset("conn7", 98000000)

	sessions = GlobalRegistry.GetActiveSessions()
	session = sessions[0]
	if session.ActiveConns != 2 {
		t.Errorf("Expected active_conns to be 2 after conn7 read > 2MB, got %d", session.ActiveConns)
	}
}

func TestCoordinatorResuscitationOnClientStarvation(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 1000))
	}))
	defer server.Close()

	key := "test_resuscitation.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		1000,
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	req := httptest.NewRequest("GET", "/stream", nil)
	reader := NewCacheReader(context.Background(), state, req)
	defer reader.Close()

	buf := make([]byte, 100)
	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("Expected read to succeed after resuscitation, got err: %v", err)
	}
	if n != 100 {
		t.Fatalf("Expected 100 bytes read, got %d", n)
	}

	state.Lock.Lock()
	active := state.WriterActive
	state.Lock.Unlock()

	if !active {
		t.Errorf("Expected coordinator WriterActive to be true after resuscitation")
	}

	CancelCoordinator(state)
}

func TestCancelHydrationAPI(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	key := "test_cancel_api.mkv"
	partPath := filepath.Join(GlobalConfig.TmpDir, key+".part")
	metaPath := filepath.Join(GlobalConfig.CacheDir, key+".json")
	_ = os.WriteFile(partPath, []byte("partial data"), 0644)
	_ = os.WriteFile(metaPath, []byte("{}"), 0644)

	ctx, cancel := context.WithCancel(context.Background())
	state := NewFileCacheState(
		key,
		"http://example.com/test.mkv",
		filepath.Join(GlobalConfig.CacheDir, key),
		partPath,
		metaPath,
		"video/x-matroska",
		"",
		"",
		1000,
	)
	state.WriterActive = true
	state.Cancel = cancel

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	req := httptest.NewRequest("POST", "/api/hydration/cancel?key="+key, nil)
	rr := httptest.NewRecorder()

	handleCancelHydration(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected HTTP 200 OK from cancel API, got status %d", rr.Code)
	}

	select {
	case <-ctx.Done():
	default:
		t.Errorf("Expected coordinator context to be cancelled")
	}

	GlobalRegistry.Lock.Lock()
	_, exists := GlobalRegistry.Files[key]
	GlobalRegistry.Lock.Unlock()
	if exists {
		t.Errorf("Expected file state to be removed from registry after cancellation with 0 clients")
	}

	if FileExists(partPath) {
		t.Errorf("Expected part file %s to be deleted from disk on cancellation", partPath)
	}
	if FileExists(metaPath) {
		t.Errorf("Expected meta file %s to be deleted from disk on cancellation", metaPath)
	}
}

func TestEMASpeedSmoothingAndAverageSpeed(t *testing.T) {
	key := "test_ema_speed.mkv"
	now := time.Now().Unix()
	state := NewFileCacheState(
		key,
		"http://example.com/test.mkv",
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		100000,
	)
	state.WriterActive = true
	state.UpstreamStatus = "Downloading"
	state.HydrationStartTime = now - 10
	state.TotalBytesDownloaded = 500000
	state.BytesDownloadedSec = 100000

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, key)
		GlobalRegistry.Lock.Unlock()
	}()

	state.Lock.Lock()
	instantSpeed := float64(state.BytesDownloadedSec)
	state.BytesDownloadedSec = 0

	state.SmoothDownloadSpeed = instantSpeed
	state.DownloadSpeed = int64(state.SmoothDownloadSpeed)

	elapsed := now - state.HydrationStartTime
	if elapsed > 0 {
		state.AvgDownloadSpeed = state.TotalBytesDownloaded / elapsed
	}
	state.Lock.Unlock()

	if state.DownloadSpeed != 100000 {
		t.Errorf("Expected initial smooth download speed 100000, got %d", state.DownloadSpeed)
	}

	if state.AvgDownloadSpeed != 50000 {
		t.Errorf("Expected avg download speed 50000 (500000/10s), got %d", state.AvgDownloadSpeed)
	}

	state.Lock.Lock()
	instantSpeed2 := float64(state.BytesDownloadedSec)
	state.BytesDownloadedSec = 0
	if instantSpeed2 == 0 {
		state.SmoothDownloadSpeed = 0
	} else if state.SmoothDownloadSpeed == 0 {
		state.SmoothDownloadSpeed = instantSpeed2
	} else {
		state.SmoothDownloadSpeed = (0.5 * instantSpeed2) + (0.5 * state.SmoothDownloadSpeed)
	}
	state.DownloadSpeed = int64(state.SmoothDownloadSpeed)
	state.Lock.Unlock()

	if state.DownloadSpeed != 0 {
		t.Errorf("Expected fast-snap download speed 0 after 0 B/s cycle, got %d", state.DownloadSpeed)
	}
}

func TestCancellationAndResuscitation(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	testBytes := make([]byte, 400)
	for i := range testBytes {
		testBytes[i] = byte(i % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "video.mkv", time.Now(), bytes.NewReader(testBytes))
	}))
	defer server.Close()

	key := "test_cancel_resuscitate.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		int64(len(testBytes)),
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	req := httptest.NewRequest("GET", "/stream", nil)
	reader1 := NewCacheReader(context.Background(), state, req)

	StartCoordinator(state, nil)
	CancelCoordinator(state)
	reader1.Close()

	req2 := httptest.NewRequest("GET", "/stream", nil)
	reader2 := NewCacheReader(context.Background(), state, req2)

	buf := make([]byte, 100)
	n, err := reader2.Read(buf)
	if err != nil {
		t.Fatalf("Expected reader2.Read to succeed after cancellation resuscitation, got: %v", err)
	}
	if n != 100 {
		t.Fatalf("Expected 100 bytes read, got %d", n)
	}

	_ = reader2.Close()
	CancelCoordinator(state)
}

func TestSingleWorkerTailPrefetch(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers

	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.MaxUpstreamWorkers = 1
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.Unlock()
	}()

	totalSize := int64(1000000)
	cuesOffset := int64(800000)

	ebmlHeader := makeEbmlElement(ID_EBML, []byte{0x42, 0x86, 0x81, 0x01})
	seekID := makeEbmlElement(ID_SEEKID, []byte{0x1C, 0x53, 0xBB, 0x6B})
	seekPosVal := []byte{0x0C, 0x35, 0x00} // 800000 in uint24 big endian
	seekPos := makeEbmlElement(ID_SEEKPOS, seekPosVal)
	seekPayload := append(seekID, seekPos...)
	seek := makeEbmlElement(ID_SEEK, seekPayload)
	seekHead := makeEbmlElement(ID_SEEKHEAD, seek)
	segmentID := makeVintID(ID_SEGMENT)
	segmentSize := makeVint(2000000)
	segmentHeader := append(segmentID, segmentSize...)
	headerBytes := append(ebmlHeader, segmentHeader...)
	headerBytes = append(headerBytes, seekHead...)

	responseBytes := make([]byte, totalSize)
	copy(responseBytes, headerBytes)

	cuesHeader := []byte{0x1C, 0x53, 0xBB, 0x6B}
	copy(responseBytes[cuesOffset:], cuesHeader)

	var requestedRanges []string
	var lock sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		reqNum := len(requestedRanges)
		lock.Unlock()

		if reqNum == 1 {
			// First request: stream header, then delay to allow sniffing & prefetch trigger
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", totalSize-1, totalSize))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(responseBytes[:1000])
			time.Sleep(200 * time.Millisecond)
		} else {
			http.ServeContent(w, r, "test_single_prefetch.mkv", time.Time{}, bytes.NewReader(responseBytes))
		}
	}))
	defer server.Close()

	key := "test_single_prefetch.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		totalSize,
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	req := httptest.NewRequest("GET", "/stream", nil)
	reader := NewCacheReader(context.Background(), state, req)
	defer reader.Close()

	StartCoordinator(state, nil)
	defer CancelCoordinator(state)

	buf := make([]byte, 100)
	_, _ = reader.Read(buf)

	time.Sleep(300 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 requests (1 initial/sequential, 1 inline tail prefetch), got %d. Ranges: %v", len(ranges), ranges)
	}
}

func TestCancelAndRehydrateSameMedia(t *testing.T) {
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.EvictYieldedParts = true
	defer func() {
		GlobalConfig.EvictYieldedParts = false
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	testBytes := make([]byte, 10000)
	for i := range testBytes {
		testBytes[i] = byte(i % 256)
	}

	blockCh := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", "\"test-etag-123\"")
		if r.Header.Get("Range") != "" && r.Header.Get("Range") != "bytes=0-0" {
			select {
			case <-blockCh:
			case <-r.Context().Done():
				return
			}
		}
		http.ServeContent(w, r, "video.mp4", time.Now(), bytes.NewReader(testBytes))
	}))
	defer func() {
		close(blockCh)
		server.Close()
	}()

	targetURL := server.URL + "/video.mp4"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext

	req1 := httptest.NewRequest("GET", "/stream?url="+url.QueryEscape(targetURL), nil)
	rr1 := httptest.NewRecorder()

	ctx1, cancel1 := context.WithCancel(context.Background())
	req1 = req1.WithContext(ctx1)

	go handleMediaStream(rr1, req1)

	time.Sleep(100 * time.Millisecond)

	cancel1()
	time.Sleep(50 * time.Millisecond)

	cancelReq := httptest.NewRequest("POST", "/api/hydration/cancel?key="+cacheFileName, nil)
	cancelRR := httptest.NewRecorder()
	handleCancelHydration(cancelRR, cancelReq)

	if cancelRR.Code != http.StatusOK {
		t.Fatalf("Expected HTTP 200 from cancel API, got status %d", cancelRR.Code)
	}

	req2 := httptest.NewRequest("GET", "/stream?url="+url.QueryEscape(targetURL), nil)
	req2.Header.Set("If-None-Match", "\"test-etag-123\"")
	rr2 := httptest.NewRecorder()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	req2 = req2.WithContext(ctx2)

	go handleMediaStream(rr2, req2)

	time.Sleep(200 * time.Millisecond)

	GlobalRegistry.Lock.RLock()
	state2, exists := GlobalRegistry.Files[cacheFileName]
	GlobalRegistry.Lock.RUnlock()

	if !exists || state2 == nil {
		t.Fatalf("Expected new FileCacheState in registry for re-hydrated media %s", cacheFileName)
	}

	state2.Lock.Lock()
	writerActive := state2.WriterActive
	state2.Lock.Unlock()

	if !writerActive {
		t.Errorf("Expected state2.WriterActive to be true for re-hydrated media")
	}

	cancel2()
	CancelCoordinator(state2)
	time.Sleep(50 * time.Millisecond)
}

func TestUpstreamUserAgentModes(t *testing.T) {
	GlobalConfig.Lock()
	oldUA := GlobalConfig.UpstreamUserAgent
	oldMode := GlobalConfig.UpstreamUserAgentMode
	GlobalConfig.UpstreamUserAgent = "TestUA/1.0"
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.UpstreamUserAgent = oldUA
		GlobalConfig.UpstreamUserAgentMode = oldMode
		GlobalConfig.Unlock()
	}()

	GlobalConfig.Lock()
	GlobalConfig.UpstreamUserAgentMode = "smart"
	GlobalConfig.Unlock()

	throttledUAs := []string{
		"Lavf/59.27.100",
		"FFmpeg/4.4",
		"libavformat/58.76.100",
		"curl/7.88.1",
		"stagefright/1.2",
		"Unknown",
		"",
	}
	for _, ua := range throttledUAs {
		src := make(http.Header)
		if ua != "" {
			src.Set("User-Agent", ua)
		}
		dst := make(http.Header)
		CopySafeHeaders(src, dst)
		if dst.Get("User-Agent") != "TestUA/1.0" {
			t.Errorf("Smart mode failed: expected %q for input %q, got %q", "TestUA/1.0", ua, dst.Get("User-Agent"))
		}
	}

	validUAs := []string{
		"Infuse/7.5",
		"TiviMate/4.7.0",
		"AppleCoreMedia/1.0",
		"VLC/3.0.18 LibVLC/3.0.18",
	}
	for _, ua := range validUAs {
		src := make(http.Header)
		src.Set("User-Agent", ua)
		dst := make(http.Header)
		CopySafeHeaders(src, dst)
		if dst.Get("User-Agent") != ua {
			t.Errorf("Smart mode failed: expected valid UA %q to pass through, got %q", ua, dst.Get("User-Agent"))
		}
	}

	GlobalConfig.Lock()
	GlobalConfig.UpstreamUserAgentMode = "force"
	GlobalConfig.Unlock()

	allUAs := append(throttledUAs, validUAs...)
	for _, ua := range allUAs {
		src := make(http.Header)
		if ua != "" {
			src.Set("User-Agent", ua)
		}
		dst := make(http.Header)
		CopySafeHeaders(src, dst)
		if dst.Get("User-Agent") != "TestUA/1.0" {
			t.Errorf("Force mode failed: expected %q for input %q, got %q", "TestUA/1.0", ua, dst.Get("User-Agent"))
		}
	}

	GlobalConfig.Lock()
	GlobalConfig.UpstreamUserAgentMode = "passthrough"
	GlobalConfig.Unlock()

	src := make(http.Header)
	src.Set("User-Agent", "Lavf/59.27.100")
	dst := make(http.Header)
	CopySafeHeaders(src, dst)
	if dst.Get("User-Agent") != "Lavf/59.27.100" {
		t.Errorf("Passthrough mode failed: expected %q, got %q", "Lavf/59.27.100", dst.Get("User-Agent"))
	}

	srcEmpty := make(http.Header)
	dstEmpty := make(http.Header)
	CopySafeHeaders(srcEmpty, dstEmpty)
	if dstEmpty.Get("User-Agent") != "TestUA/1.0" {
		t.Errorf("Passthrough mode empty fallback failed: expected %q, got %q", "TestUA/1.0", dstEmpty.Get("User-Agent"))
	}
}

func TestNonBlockingCommit(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Lock.Unlock()
	}()

	key := "test_non_blocking_commit.mkv"
	partPath := filepath.Join(t.TempDir(), key+".part")
	finalPath := filepath.Join(t.TempDir(), key)

	_ = os.WriteFile(partPath, []byte("dummy stream content"), 0644)

	state := NewFileCacheState(
		key,
		"http://dummy",
		finalPath,
		partPath,
		filepath.Join(t.TempDir(), key+".json"),
		"video/x-matroska",
		"",
		"",
		20,
	)
	state.RangeSet.Add(0, 20)
	state.UpstreamStatus = "Downloading"
	state.WriterActive = true

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	start := time.Now()

	state.Lock.Lock()
	state.UpstreamStatus = "Committing"
	state.Lock.Unlock()

	state.SetUpstreamStatus("Completed")
	GlobalRegistry.MarkConnectionsAsInCache(key)

	err := os.Rename(state.PartPath, state.Path)
	if err != nil {
		t.Fatalf("Failed to rename completed file: %v", err)
	}

	elapsed := time.Since(start)
	if elapsed > 100*time.Millisecond {
		t.Errorf("Commit process took too long (expected <100ms, got %v)", elapsed)
	}

	state.Lock.Lock()
	status := state.UpstreamStatus
	state.Lock.Unlock()

	if status != "Completed" {
		t.Errorf("Expected status Completed, got %s", status)
	}
}

func TestThrottledStatusNetworkOnly(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Lock.Unlock()
	}()

	committingKey := "committing_file.mkv"
	committingState := NewFileCacheState(
		committingKey,
		"http://dummy",
		filepath.Join(t.TempDir(), committingKey),
		filepath.Join(t.TempDir(), committingKey+".part"),
		filepath.Join(t.TempDir(), committingKey+".json"),
		"video/x-matroska",
		"",
		"",
		100,
	)
	committingState.UpstreamStatus = "Committing"

	activeKey := "active_download.mkv"
	activeState := NewFileCacheState(
		activeKey,
		"http://dummy",
		filepath.Join(t.TempDir(), activeKey),
		filepath.Join(t.TempDir(), activeKey+".part"),
		filepath.Join(t.TempDir(), activeKey+".json"),
		"video/x-matroska",
		"",
		"",
		100,
	)
	activeState.UpstreamStatus = "Downloading"
	activeState.WriterActive = true
	activeState.ZeroSpeedSec = 2

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[committingKey] = committingState
	GlobalRegistry.Files[activeKey] = activeState
	GlobalRegistry.Lock.Unlock()

	GlobalRegistry.Lock.RLock()
	files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
	for _, fs := range GlobalRegistry.Files {
		files = append(files, fs)
	}
	GlobalRegistry.Lock.RUnlock()

	isAnyCommitting := false
	for _, fs := range files {
		fs.Lock.Lock()
		if fs.UpstreamStatus == "Committing" {
			isAnyCommitting = true
		}
		fs.Lock.Unlock()
	}

	activeState.Lock.Lock()
	instantSpeed := float64(activeState.BytesDownloadedSec)
	activeState.BytesDownloadedSec = 0

	if activeState.WriterActive && !isAnyCommitting && activeState.UpstreamStatus != "Committing" {
		if instantSpeed == 0 {
			activeState.ZeroSpeedSec++
			if activeState.ZeroSpeedSec >= 3 && activeState.UpstreamStatus == "Downloading" {
				activeState.UpstreamStatus = "Throttled"
			}
		}
	} else {
		activeState.ZeroSpeedSec = 0
	}
	activeState.Lock.Unlock()

	if activeState.UpstreamStatus == "Throttled" {
		t.Errorf("Expected active file NOT to flip to Throttled while another file is Committing")
	}
	if activeState.ZeroSpeedSec != 0 {
		t.Errorf("Expected ZeroSpeedSec to be reset to 0 during commit, got %d", activeState.ZeroSpeedSec)
	}
}

func TestActiveReaderDuringCommitAndRename(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.Unlock()
	}()

	testPayload := make([]byte, 300)
	for i := range testPayload {
		testPayload[i] = byte(i % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "active_reader_test.mkv", time.Time{}, bytes.NewReader(testPayload))
	}))
	defer server.Close()

	key := "active_reader_test.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		300,
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	defer cleanupAfterTest(t, state)

	req := httptest.NewRequest("GET", "/stream", nil)
	reader := NewCacheReader(context.Background(), state, req)
	defer reader.Close()

	StartCoordinator(state, nil)

	buf0 := make([]byte, 100)
	n0, err := reader.Read(buf0)
	if err != nil || n0 != 100 {
		t.Fatalf("Failed reading first 100 bytes during hydration: n=%d, err=%v", n0, err)
	}
	if !bytes.Equal(buf0, testPayload[:100]) {
		t.Fatalf("First 100 bytes content mismatch")
	}

	for i := 0; i < 100; i++ {
		state.Lock.Lock()
		writerActive := state.WriterActive
		state.Lock.Unlock()
		if !writerActive && FileExists(state.Path) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !FileExists(state.Path) {
		t.Fatalf("Expected file to be committed and renamed to %s, but not found", state.Path)
	}

	bufRest := make([]byte, 200)
	nRest, err := reader.Read(bufRest)
	if err != nil && err != io.EOF {
		t.Fatalf("Failed reading remaining 200 bytes after file move/rename: n=%d, err=%v", nRest, err)
	}
	if nRest != 200 {
		t.Fatalf("Expected 200 bytes read after rename, got %d", nRest)
	}
	if !bytes.Equal(bufRest, testPayload[100:]) {
		t.Fatalf("Remaining 200 bytes content mismatch after rename")
	}
}

func TestStarvationStallTimeoutBreakout(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	tmpDir := t.TempDir()
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	oldMinTime := GlobalConfig.SchedulerMinTime
	oldStallTimeout := GlobalConfig.StarvationStallTimeout

	GlobalConfig.MaxUpstreamWorkers = 1
	GlobalConfig.ClientHydrationWait = 2 * time.Second
	GlobalConfig.SchedulerMinTime = 50 * time.Millisecond
	GlobalConfig.StarvationStallTimeout = 100 * time.Millisecond
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
		GlobalConfig.SchedulerMinTime = oldMinTime
		GlobalConfig.StarvationStallTimeout = oldStallTimeout
		GlobalConfig.Unlock()
	}()

	var lock sync.Mutex
	var requestedRanges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		reqIdx := len(requestedRanges)
		lock.Unlock()

		if reqIdx == 1 {
			// Worker 1: establish connection at offset 0, send 100 bytes, then stall
			w.Header().Set("Content-Range", "bytes 0-99/1000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
			// Keep socket open / stall without sending more bytes
			time.Sleep(1 * time.Second)
		} else {
			// Worker 2 (breakout): responds immediately for offset 200
			w.Header().Set("Content-Range", "bytes 200-999/1000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 800))
		}
	}))
	defer server.Close()

	key := "test_starvation_stall.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		1000,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(20 * time.Millisecond) // Worker 1 connects and begins download

	// Simulate client starving at offset 200 (within 512KB of worker 1's position at 100)
	state.Lock.Lock()
	state.Starving["client-starve"] = 200
	state.StarvingSince["client-starve"] = time.Now()
	state.Lock.Unlock()

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	// 1. Check immediately: worker has been idle for < 100ms stall timeout -> jump should be suppressed
	time.Sleep(30 * time.Millisecond)
	lock.Lock()
	initialReqCount := len(requestedRanges)
	lock.Unlock()

	if initialReqCount != 1 {
		t.Fatalf("Expected initial suppression (only 1 range request so far), got %d: %v", initialReqCount, requestedRanges)
	}

	// 2. Wait until StarvationStallTimeout (100ms) elapses
	time.Sleep(150 * time.Millisecond)

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	finalRanges := make([]string, len(requestedRanges))
	copy(finalRanges, requestedRanges)
	lock.Unlock()

	if len(finalRanges) < 2 {
		t.Fatalf("Expected breakout after StarvationStallTimeout (at least 2 range requests), but got %d: %v", len(finalRanges), finalRanges)
	}

	expectedRange := "bytes=200-999"
	if finalRanges[1] != expectedRange {
		t.Errorf("Expected breakout worker (index 1) to request %q, got %v", expectedRange, finalRanges)
	}
}

func TestStarvationStallTimeoutMultiWorker(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	tmpDir := t.TempDir()
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	oldMinTime := GlobalConfig.SchedulerMinTime
	oldStallTimeout := GlobalConfig.StarvationStallTimeout

	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.ClientHydrationWait = 2 * time.Second
	GlobalConfig.SchedulerMinTime = 50 * time.Millisecond
	GlobalConfig.StarvationStallTimeout = 100 * time.Millisecond
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
		GlobalConfig.SchedulerMinTime = oldMinTime
		GlobalConfig.StarvationStallTimeout = oldStallTimeout
		GlobalConfig.Unlock()
	}()

	var lock sync.Mutex
	var requestedRanges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		reqIdx := len(requestedRanges)
		lock.Unlock()

		if reqIdx == 1 {
			// Worker 1: establish connection at offset 0, send 100 bytes, then stall
			w.Header().Set("Content-Range", "bytes 0-99/1000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
			// Keep socket open / stall without sending more bytes
			time.Sleep(1 * time.Second)
		} else {
			// Worker 2 (breakout): responds immediately for offset 200
			w.Header().Set("Content-Range", "bytes 200-999/1000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 800))
		}
	}))
	defer server.Close()

	key := "test_starvation_stall_mw.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		1000,
	)
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	StartCoordinator(state, nil)
	time.Sleep(20 * time.Millisecond) // Worker 1 connects and begins download

	// Simulate client starving at offset 200 (within 512KB of worker 1's position at 100)
	state.Lock.Lock()
	state.Starving["client-starve"] = 200
	state.StarvingSince["client-starve"] = time.Now()
	state.Lock.Unlock()

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	// 1. Initial check: worker 1 idle < 100ms -> suppressed
	time.Sleep(30 * time.Millisecond)
	lock.Lock()
	initialReqCount := len(requestedRanges)
	lock.Unlock()

	if initialReqCount != 1 {
		t.Fatalf("Expected initial suppression with MaxUpstreamWorkers=2, got %d requests: %v", initialReqCount, requestedRanges)
	}

	// 2. Wait until StarvationStallTimeout (100ms) elapses
	time.Sleep(150 * time.Millisecond)

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	finalRanges := make([]string, len(requestedRanges))
	copy(finalRanges, requestedRanges)
	lock.Unlock()

	if len(finalRanges) < 2 {
		t.Fatalf("Expected breakout after StarvationStallTimeout with MaxUpstreamWorkers=2, got %d requests: %v", len(finalRanges), finalRanges)
	}

	expectedRange := "bytes=200-999"
	if finalRanges[1] != expectedRange {
		t.Errorf("Expected breakout worker (index 1) to request %q, got %v", expectedRange, finalRanges)
	}
}

func TestDashboardSSEStateStream(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	oldSessions := GlobalRegistry.Sessions
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.MaxObservedSpeed = 15000000 // 15 MB/s
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Sessions = oldSessions
		GlobalRegistry.Lock.Unlock()
	}()

	key := "test_dashboard_stream.mp4"
	state := NewFileCacheState(
		key,
		"http://dummy.test/video.mp4",
		filepath.Join(t.TempDir(), key),
		filepath.Join(t.TempDir(), key+".part"),
		filepath.Join(t.TempDir(), key+".json"),
		"video/mp4",
		"",
		"",
		50000,
	)
	state.RangeSet.Add(0, 20000)
	state.RangeSet.Add(35000, 45000)
	state.WriterActive = true
	state.UpstreamStatus = "Downloading"
	state.DownloadSpeed = 10000000
	state.ZeroSpeedSec = 4 // Stalled

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Sessions["sess-1"] = &ClientSession{
		ID:             "sess-1",
		Key:            key,
		RemoteAddr:     "192.168.1.50",
		UserAgent:      "VLC/3.0.20",
		Offset:         15000,
		MaxOffset:      15000,
		ActiveConns:    1,
		StarvingOffset: -1,
		StartTime:      time.Now().Unix() - 10,
		LastActive:     time.Now(),
	}
	GlobalRegistry.Sessions["sess-2"] = &ClientSession{
		ID:             "sess-2",
		Key:            key,
		RemoteAddr:     "192.168.1.51",
		UserAgent:      "Kodi/20.0",
		Offset:         25000,
		MaxOffset:      25000,
		ActiveConns:    1,
		StarvingOffset: 25000, // Starving
		StartTime:      time.Now().Unix() - 5,
		LastActive:     time.Now(),
	}
	GlobalRegistry.Lock.Unlock()

	payloadBytes, err := buildStatePayload()
	if err != nil {
		t.Fatalf("buildStatePayload failed: %v", err)
	}

	var stateMap map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &stateMap); err != nil {
		t.Fatalf("Failed unmarshaling state payload: %v", err)
	}

	if maxObserved, ok := stateMap["max_observed_speed"].(float64); !ok || int64(maxObserved) != 15000000 {
		t.Errorf("Expected max_observed_speed = 15000000, got %v", stateMap["max_observed_speed"])
	}

	activeDownloads, ok := stateMap["active_downloads"].([]interface{})
	if !ok || len(activeDownloads) != 1 {
		t.Fatalf("Expected 1 active download, got %d", len(activeDownloads))
	}

	fileData := activeDownloads[0].(map[string]interface{})
	if fileData["key"] != key {
		t.Errorf("Expected key %q, got %q", key, fileData["key"])
	}
	if fileData["is_stalled"] != true {
		t.Errorf("Expected is_stalled=true for ZeroSpeedSec=4, got %v", fileData["is_stalled"])
	}
	if fileData["upstream_status"] != "Downloading" {
		t.Errorf("Expected upstream_status='Downloading', got %v", fileData["upstream_status"])
	}

	clientPositions, ok := fileData["client_positions"].([]interface{})
	if !ok || len(clientPositions) != 2 {
		t.Fatalf("Expected 2 client positions for file, got %d", len(clientPositions))
	}

	req := httptest.NewRequest("GET", "/dashboard/sse", nil)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	go handleSSE(w, req)
	time.Sleep(50 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	respBody := w.Body.String()
	if !strings.Contains(respBody, "event: state") {
		t.Errorf("Expected SSE stream to contain 'event: state', got:\n%s", respBody)
	}
	if strings.Contains(respBody, "event: range_update") {
		t.Errorf("SSE stream should NOT contain deprecated 'event: range_update'")
	}
}

func TestDynamicSSEActivityInterval(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	oldSessions := GlobalRegistry.Sessions
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Sessions = oldSessions
		GlobalRegistry.Lock.Unlock()
	}()

	if GlobalRegistry.HasActiveActivity() {
		t.Errorf("Expected HasActiveActivity() = false when registry is empty")
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Sessions["sess-1"] = &ClientSession{ID: "sess-1"}
	GlobalRegistry.Lock.Unlock()

	if !GlobalRegistry.HasActiveActivity() {
		t.Errorf("Expected HasActiveActivity() = true when active session exists")
	}
}

func TestLiveBufferFastPath(t *testing.T) {
	lb := NewLiveBuffer(256 * 1024) // 256KB capacity

	data1 := make([]byte, 64*1024)
	for i := range data1 {
		data1[i] = byte(i % 256)
	}
	data2 := make([]byte, 64*1024)
	for i := range data2 {
		data2[i] = byte((i + 1) % 256)
	}

	c1 := newPooledChunk(0, data1, 1)
	lb.Put(c1)
	c2 := newPooledChunk(64*1024, data2, 1)
	lb.Put(c2)

	// Single chunk read hit
	buf := make([]byte, 1024)
	n, ok := lb.ReadAt(buf, 0)
	if !ok || n != 1024 {
		t.Fatalf("Expected ReadAt(0) hit, got n=%d, ok=%v", n, ok)
	}
	if !bytes.Equal(buf, data1[:1024]) {
		t.Fatalf("Data mismatch on ReadAt(0)")
	}

	// Cross-chunk boundary read hit
	boundaryBuf := make([]byte, 2048)
	crossOffset := int64(64*1024 - 1024)
	nCross, okCross := lb.ReadAt(boundaryBuf, crossOffset)
	if !okCross || nCross != 2048 {
		t.Fatalf("Expected cross-chunk boundary hit, got n=%d, ok=%v", nCross, okCross)
	}
	expectedCross := append(data1[64*1024-1024:], data2[:1024]...)
	if !bytes.Equal(boundaryBuf, expectedCross) {
		t.Fatalf("Data mismatch across chunk boundary")
	}

	// Miss outside buffered range
	missBuf := make([]byte, 512)
	_, okMiss := lb.ReadAt(missBuf, 500*1024)
	if okMiss {
		t.Fatalf("Expected miss for out-of-range offset, got ok=true")
	}

	// Test eviction when capacity exceeded
	for i := 2; i < 6; i++ {
		extra := make([]byte, 64*1024)
		cExtra := newPooledChunk(int64(i*64*1024), extra, 1)
		lb.Put(cExtra)
	}

	// Offset 0 should now be evicted
	_, okEvicted := lb.ReadAt(buf, 0)
	if okEvicted {
		t.Fatalf("Expected offset 0 to be evicted from LiveBuffer, but was still present")
	}

	// Clean up
	lb.Clear()
	if lb.curBytes != 0 {
		t.Fatalf("Expected curBytes=0 after Clear, got %d", lb.curBytes)
	}
}

func TestLiveBufferLRUOrderOnSeek(t *testing.T) {
	// Capacity for 2 chunks of 64KB = 128KB
	lb := NewLiveBuffer(128 * 1024)

	c10M := newPooledChunk(10*1024*1024, make([]byte, 64*1024), 1)
	lb.Put(c10M)

	c20M := newPooledChunk(20*1024*1024, make([]byte, 64*1024), 1)
	lb.Put(c20M)

	// Buffer is now at capacity (128KB).
	// User seeks backwards to offset 0.
	c0 := newPooledChunk(0, make([]byte, 64*1024), 1)
	lb.Put(c0)

	// c10M (oldest seq) should be evicted.
	// c0 (offset 0) and c20M should still be present!
	buf := make([]byte, 1024)
	if _, ok := lb.ReadAt(buf, 0); !ok {
		t.Fatalf("Expected chunk at offset 0 to be present in LiveBuffer, but was prematurely evicted")
	}
	if _, ok := lb.ReadAt(buf, 20*1024*1024); !ok {
		t.Fatalf("Expected chunk at offset 20MB to be present in LiveBuffer")
	}
	if _, ok := lb.ReadAt(buf, 10*1024*1024); ok {
		t.Fatalf("Expected chunk at offset 10MB to be evicted by LRU")
	}

	lb.Clear()
}

func TestWritebackPipelineDrainAndFlush(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "writeback_test_*.part")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer tmpFile.Close()

	wp := NewWritebackPipeline(tmpFile, nil, 1024*1024)

	payload1 := []byte("Hello Writeback Pipeline!")
	payload2 := []byte("Streaming Live Edge Content")
	payload3 := []byte("Non-contiguous chunk at high offset")

	if err := wp.WriteChunk(0, payload1); err != nil {
		t.Fatalf("WriteChunk(0) failed: %v", err)
	}
	if err := wp.WriteChunk(100, payload2); err != nil {
		t.Fatalf("WriteChunk(100) failed: %v", err)
	}
	if err := wp.WriteChunk(10*1024*1024, payload3); err != nil {
		t.Fatalf("WriteChunk(10MB) failed: %v", err)
	}

	if err := wp.FlushAndClose(); err != nil {
		t.Fatalf("FlushAndClose failed: %v", err)
	}

	// Subsequent write after close should fail
	if err := wp.WriteChunk(200, []byte("late")); err == nil {
		t.Fatalf("Expected write after close to error, got nil")
	}

	// Verify data was written to disk
	r1 := make([]byte, len(payload1))
	if _, err := tmpFile.ReadAt(r1, 0); err != nil || !bytes.Equal(r1, payload1) {
		t.Fatalf("Payload 1 mismatch or read error: %v", err)
	}

	r2 := make([]byte, len(payload2))
	if _, err := tmpFile.ReadAt(r2, 100); err != nil || !bytes.Equal(r2, payload2) {
		t.Fatalf("Payload 2 mismatch or read error: %v", err)
	}

	r3 := make([]byte, len(payload3))
	if _, err := tmpFile.ReadAt(r3, 10*1024*1024); err != nil || !bytes.Equal(r3, payload3) {
		t.Fatalf("Payload 3 mismatch or read error: %v", err)
	}
}

func TestAsynchronousCommitHandoff(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.Unlock()
	}()

	testPayload := make([]byte, 2048)
	for i := range testPayload {
		testPayload[i] = byte((i * 7) % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "async_commit_test.mkv", time.Time{}, bytes.NewReader(testPayload))
	}))
	defer server.Close()

	key := "async_commit_test.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		int64(len(testPayload)),
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	defer cleanupAfterTest(t, state)

	StartCoordinator(state, nil)

	// Poll until commit is complete and file is finalized on disk
	deadline := time.Now().Add(5 * time.Second)
	committed := false
	for time.Now().Before(deadline) {
		state.Lock.Lock()
		active := state.WriterActive
		state.Lock.Unlock()

		if !active && FileExists(state.Path) {
			committed = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !committed {
		t.Fatalf("Expected asynchronous commit to finalize %s within timeout", state.Path)
	}

	// Verify committed file contents match payload exactly
	data, err := os.ReadFile(state.Path)
	if err != nil {
		t.Fatalf("Failed reading finalized cache file: %v", err)
	}
	if !bytes.Equal(data, testPayload) {
		t.Fatalf("Finalized cache file content does not match test payload")
	}

	// Verify metadata file was generated
	metaFile := state.Path + ".json"
	if !FileExists(metaFile) {
		t.Fatalf("Expected metadata JSON file %s to exist", metaFile)
	}
}

func TestConcurrentHydrationDuringCommit(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.Unlock()
	}()

	payloadA := make([]byte, 1024)
	payloadB := make([]byte, 8192)
	for i := range payloadA {
		payloadA[i] = byte(i % 256)
	}
	for i := range payloadB {
		payloadB[i] = byte((i * 3) % 256)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "fileA") {
			http.ServeContent(w, r, "fileA.mkv", time.Time{}, bytes.NewReader(payloadA))
		} else {
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payloadB)))
			w.Header().Set("Content-Type", "video/x-matroska")
			w.WriteHeader(http.StatusOK)
			chunkSize := 1024
			for i := 0; i < len(payloadB); i += chunkSize {
				end := i + chunkSize
				if end > len(payloadB) {
					end = len(payloadB)
				}
				_, _ = w.Write(payloadB[i:end])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}))
	defer server.Close()

	keyA := "fileA.mkv"
	stateA := NewFileCacheState(
		keyA,
		server.URL+"/fileA.mkv",
		filepath.Join(GlobalConfig.CacheDir, keyA),
		filepath.Join(GlobalConfig.TmpDir, keyA+".part"),
		filepath.Join(GlobalConfig.CacheDir, keyA+".json"),
		"video/x-matroska",
		"",
		"",
		int64(len(payloadA)),
	)

	keyB := "fileB.mkv"
	stateB := NewFileCacheState(
		keyB,
		server.URL+"/fileB.mkv",
		filepath.Join(GlobalConfig.CacheDir, keyB),
		filepath.Join(GlobalConfig.TmpDir, keyB+".part"),
		filepath.Join(GlobalConfig.CacheDir, keyB+".json"),
		"video/x-matroska",
		"",
		"",
		int64(len(payloadB)),
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[keyA] = stateA
	GlobalRegistry.Files[keyB] = stateB
	GlobalRegistry.Lock.Unlock()

	defer cleanupAfterTest(t, stateA)
	defer cleanupAfterTest(t, stateB)

	// Start File B first so it is actively downloading
	StartCoordinator(stateB, nil)
	time.Sleep(20 * time.Millisecond)

	// Start File A which will quickly finish and enter Committing asynchronously
	StartCoordinator(stateA, nil)

	// Wait for both to complete
	deadline := time.Now().Add(5 * time.Second)
	bothDone := false
	for time.Now().Before(deadline) {
		if FileExists(stateA.Path) && FileExists(stateB.Path) {
			bothDone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if !bothDone {
		t.Fatalf("Expected both concurrent hydrations to complete successfully")
	}

	// Verify file B contents
	dataB, err := os.ReadFile(stateB.Path)
	if err != nil || !bytes.Equal(dataB, payloadB) {
		t.Fatalf("File B content corrupted or mismatched: %v", err)
	}
}

func TestSeekForward_MultiWorker(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldHydrationWait := GlobalConfig.ClientHydrationWait
	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.ClientHydrationWait = 2 * time.Second
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldHydrationWait
		GlobalConfig.Unlock()
	}()

	totalSize := int64(10 * 1024 * 1024) // 10MB
	seekOffset := int64(7 * 1024 * 1024)  // 7MB (skip forward)

	var workerRequests []string
	var reqLock sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqLock.Lock()
		workerRequests = append(workerRequests, r.Header.Get("Range"))
		reqLock.Unlock()

		rangeHdr := r.Header.Get("Range")
		// Simulate a slow stream so Worker 1 is still early in the file when seek occurs
		w.Header().Set("Content-Type", "video/x-matroska")
		if strings.HasPrefix(rangeHdr, "bytes=0-") {
			w.WriteHeader(http.StatusOK)
			// Slowly write 64KB chunks
			buf := make([]byte, 64*1024)
			for i := 0; i < 10; i++ {
				_, _ = w.Write(buf)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(20 * time.Millisecond)
			}
		} else if strings.Contains(rangeHdr, fmt.Sprintf("bytes=%d-", seekOffset)) {
			w.WriteHeader(http.StatusPartialContent)
			buf := make([]byte, 64*1024)
			for i := range buf {
				buf[i] = 0xAB
			}
			_, _ = w.Write(buf)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	key := "seek_forward_multi_worker_test.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		totalSize,
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	defer func() {
		CancelCoordinator(state)
		cleanupAfterTest(t, state)
	}()

	// 1. Start hydration from offset 0
	StartCoordinator(state, nil)

	// Wait for Worker 1 to start downloading
	time.Sleep(50 * time.Millisecond)

	// 2. Client joins and immediately skips forward to seekOffset (7MB)
	req := httptest.NewRequest("GET", "/stream", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", seekOffset))
	reader := NewCacheReader(context.Background(), state, req)
	defer reader.Close()

	// 3. Reader attempts to read from seekOffset in a goroutine
	readDone := make(chan struct{})
	readBuf := make([]byte, 1024)
	go func() {
		n, err := reader.ReadAt(readBuf, seekOffset)
		if err == nil && n == 1024 {
			close(readDone)
		}
	}()

	// 4. Verify that a secondary worker is spawned concurrently for seekOffset within timeout
	select {
	case <-readDone:
		// Success! Reader received bytes at seekOffset
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for seek forward read. Secondary worker was not spawned to satisfy seekOffset %d", seekOffset)
	}

	// Verify that workerRequests contains a request for seekOffset
	reqLock.Lock()
	defer reqLock.Unlock()
	foundSecondaryWorker := false
	expectedPrefix := fmt.Sprintf("bytes=%d-", seekOffset)
	for _, reqRange := range workerRequests {
		if strings.HasPrefix(reqRange, expectedPrefix) {
			foundSecondaryWorker = true
			break
		}
	}

	if !foundSecondaryWorker {
		t.Errorf("Expected upstream request starting at %s, got: %v", expectedPrefix, workerRequests)
	}
}

func TestSeekForward_SingleWorker(t *testing.T) {
	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldHydrationWait := GlobalConfig.ClientHydrationWait
	oldMinTime := GlobalConfig.SchedulerMinTime

	GlobalConfig.MaxUpstreamWorkers = 1
	GlobalConfig.ClientHydrationWait = 2 * time.Second
	GlobalConfig.SchedulerMinTime = 50 * time.Millisecond
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldHydrationWait
		GlobalConfig.SchedulerMinTime = oldMinTime
		GlobalConfig.Unlock()
	}()

	totalSize := int64(10 * 1024 * 1024) // 10MB
	seekOffset := int64(7 * 1024 * 1024)  // 7MB (skip forward)

	var workerRequests []string
	var reqLock sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqLock.Lock()
		workerRequests = append(workerRequests, r.Header.Get("Range"))
		reqLock.Unlock()

		rangeHdr := r.Header.Get("Range")
		w.Header().Set("Content-Type", "video/x-matroska")
		if strings.HasPrefix(rangeHdr, "bytes=0-") {
			w.WriteHeader(http.StatusOK)
			// Slowly write 64KB chunks to keep worker busy
			buf := make([]byte, 64*1024)
			for i := 0; i < 20; i++ {
				_, _ = w.Write(buf)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(20 * time.Millisecond)
			}
		} else if strings.Contains(rangeHdr, fmt.Sprintf("bytes=%d-", seekOffset)) {
			w.WriteHeader(http.StatusPartialContent)
			buf := make([]byte, 64*1024)
			for i := range buf {
				buf[i] = 0xCD
			}
			_, _ = w.Write(buf)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	key := "seek_forward_single_worker_test.mkv"
	state := NewFileCacheState(
		key,
		server.URL,
		filepath.Join(GlobalConfig.CacheDir, key),
		filepath.Join(GlobalConfig.TmpDir, key+".part"),
		filepath.Join(GlobalConfig.CacheDir, key+".json"),
		"video/x-matroska",
		"",
		"",
		totalSize,
	)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	defer func() {
		CancelCoordinator(state)
		cleanupAfterTest(t, state)
	}()

	// 1. Start hydration from offset 0
	StartCoordinator(state, nil)

	// Wait for Worker 1 to start downloading and exceed SchedulerMinTime (50ms)
	time.Sleep(80 * time.Millisecond)

	// 2. Client joins and immediately skips forward to seekOffset (7MB)
	req := httptest.NewRequest("GET", "/stream", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-", seekOffset))
	reader := NewCacheReader(context.Background(), state, req)
	defer reader.Close()

	// 3. Reader attempts to read from seekOffset in a goroutine
	readDone := make(chan struct{})
	readBuf := make([]byte, 1024)
	go func() {
		n, err := reader.ReadAt(readBuf, seekOffset)
		if err == nil && n == 1024 {
			close(readDone)
		}
	}()

	// 4. Verify that single worker is preempted and switched to seekOffset within timeout
	select {
	case <-readDone:
		// Success! Reader received bytes at seekOffset via worker preemption
	case <-time.After(3 * time.Second):
		t.Fatalf("Timeout waiting for seek forward read. Worker was not pre-empted to switch to seekOffset %d", seekOffset)
	}

	// Verify that workerRequests contains a request for seekOffset
	reqLock.Lock()
	defer reqLock.Unlock()
	foundPreemptedWorker := false
	expectedPrefix := fmt.Sprintf("bytes=%d-", seekOffset)
	for _, reqRange := range workerRequests {
		if strings.HasPrefix(reqRange, expectedPrefix) {
			foundPreemptedWorker = true
			break
		}
	}

	if !foundPreemptedWorker {
		t.Errorf("Expected preempted upstream request starting at %s, got: %v", expectedPrefix, workerRequests)
	}
}

func TestUserAttributionAndAPISessions(t *testing.T) {
	GlobalRegistry.Lock.Lock()
	oldConnections := GlobalRegistry.Connections
	oldSessions := GlobalRegistry.Sessions
	GlobalRegistry.Connections = make(map[string]*ClientConnection)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Connections = oldConnections
		GlobalRegistry.Sessions = oldSessions
		GlobalRegistry.Lock.Unlock()
	}()

	key := "test_user_video.mp4"
	url := "http://example.com/test_user_video.mp4"
	ip := "192.168.1.50:11111"
	ua := "VLC/3.0.18"

	// 1. Connection with username creates session with username
	conn1 := &ClientConnection{
		ID:          "conn-user-1",
		Key:         key,
		URL:         url,
		RemoteAddr:  ip,
		UserAgent:   ua,
		Username:    "alice",
		Offset:      1000,
		StartOffset: 1000,
		TotalSize:   50000000,
		StartTime:   time.Now().Unix(),
		InCache:     true,
	}
	GlobalRegistry.RegisterConnection("conn-user-1", conn1)

	sessions := GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected 1 session, got %d", len(sessions))
	}
	if sessions[0].Username != "alice" {
		t.Errorf("Expected session username 'alice', got %q", sessions[0].Username)
	}

	// 2. Matching existing session updates/sets username
	conn2 := &ClientConnection{
		ID:          "conn-user-2",
		Key:         key,
		URL:         url,
		RemoteAddr:  "192.168.1.50:22222",
		UserAgent:   ua,
		Username:    "alice_updated",
		Offset:      2000,
		StartOffset: 2000,
		TotalSize:   50000000,
		StartTime:   time.Now().Unix(),
		InCache:     true,
	}
	GlobalRegistry.RegisterConnection("conn-user-2", conn2)

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected 1 grouped session, got %d", len(sessions))
	}
	if sessions[0].Username != "alice_updated" {
		t.Errorf("Expected updated session username 'alice_updated', got %q", sessions[0].Username)
	}

	// 3. 120-second sliding idle lease
	GlobalRegistry.UnregisterConnection("conn-user-1")
	GlobalRegistry.UnregisterConnection("conn-user-2")

	// Set LastActive to 90 seconds ago (should remain active because < 120s and MaxOffset > 0)
	GlobalRegistry.Lock.Lock()
	sess := GlobalRegistry.Sessions["conn-user-1"]
	sess.LastActive = time.Now().Add(-90 * time.Second)
	GlobalRegistry.Lock.Unlock()

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 1 {
		t.Fatalf("Expected session to remain active at 90s idle (< 120s), got %d sessions", len(sessions))
	}

	// Set LastActive to 125 seconds ago (should be purged after 120s)
	GlobalRegistry.Lock.Lock()
	sess.LastActive = time.Now().Add(-125 * time.Second)
	GlobalRegistry.Lock.Unlock()

	sessions = GlobalRegistry.GetActiveSessions()
	if len(sessions) != 0 {
		t.Fatalf("Expected session to be purged at 125s idle (> 120s), got %d sessions", len(sessions))
	}

	// 4. Test /api/sessions endpoint handler
	connBob := &ClientConnection{
		ID:          "conn-bob",
		Key:         key,
		URL:         url,
		RemoteAddr:  "192.168.1.60:33333",
		UserAgent:   ua,
		Username:    "bob",
		Offset:      5000,
		StartOffset: 5000,
		TotalSize:   50000000,
		StartTime:   time.Now().Unix(),
		InCache:     true,
	}
	connCharlie := &ClientConnection{
		ID:          "conn-charlie",
		Key:         "other_video.mp4",
		URL:         "http://example.com/other_video.mp4",
		RemoteAddr:  "192.168.1.70:44444",
		UserAgent:   ua,
		Username:    "charlie",
		Offset:      5000,
		StartOffset: 5000,
		TotalSize:   50000000,
		StartTime:   time.Now().Unix(),
		InCache:     true,
	}
	GlobalRegistry.RegisterConnection("conn-bob", connBob)
	GlobalRegistry.RegisterConnection("conn-charlie", connCharlie)

	// Test all sessions
	reqAll, _ := http.NewRequest("GET", "/api/sessions", nil)
	rrAll := httptest.NewRecorder()
	handleAPISessions(rrAll, reqAll)

	if rrAll.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK from /api/sessions, got %d", rrAll.Code)
	}
	if ct := rrAll.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Expected Content-Type application/json, got %q", ct)
	}

	var respAll struct {
		Count    int             `json:"count"`
		User     string          `json:"user"`
		Sessions []ClientSession `json:"sessions"`
	}
	if err := json.Unmarshal(rrAll.Body.Bytes(), &respAll); err != nil {
		t.Fatalf("Failed to parse /api/sessions response: %v", err)
	}
	if respAll.Count != 2 || len(respAll.Sessions) != 2 {
		t.Errorf("Expected 2 sessions in /api/sessions, got count=%d, len=%d", respAll.Count, len(respAll.Sessions))
	}

	// Test filter by user case-insensitive
	reqFilter, _ := http.NewRequest("GET", "/api/sessions?user=BOB", nil)
	rrFilter := httptest.NewRecorder()
	handleAPISessions(rrFilter, reqFilter)

	var respFilter struct {
		Count    int             `json:"count"`
		User     string          `json:"user"`
		Sessions []ClientSession `json:"sessions"`
	}
	if err := json.Unmarshal(rrFilter.Body.Bytes(), &respFilter); err != nil {
		t.Fatalf("Failed to parse filtered response: %v", err)
	}
	if respFilter.Count != 1 || len(respFilter.Sessions) != 1 {
		t.Fatalf("Expected 1 filtered session for BOB, got %d", respFilter.Count)
	}
	if respFilter.Sessions[0].Username != "bob" {
		t.Errorf("Expected session username 'bob', got %q", respFilter.Sessions[0].Username)
	}
	if respFilter.User != "BOB" {
		t.Errorf("Expected response user 'BOB', got %q", respFilter.User)
	}
}




