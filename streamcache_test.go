package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	// Set up temporary configuration values for testing
	GlobalConfig = &Config{
		Port:                    ":9999",
		CacheDir:                "./test_cache/files",
		TmpDir:                  "./test_cache/tmp",
		BlockSize:               100, // small blocks for test
		CacheMaxBytes:           300,
		MinDiskFreeBytes:        1000,
		CacheRevalidateInterval: 50 * time.Millisecond, // short for test
		SchedulerMinBytes:       10,
		SchedulerMinTime:        20 * time.Millisecond,
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
	state := &FileCacheState{
		Key:         "test_cancel",
		URL:         "http://localhost/dummy",
		Path:        filepath.Join(GlobalConfig.CacheDir, "test_cancel"),
		PartPath:    filepath.Join(GlobalConfig.TmpDir, "test_cancel.part"),
		MetaPath:    filepath.Join(GlobalConfig.TmpDir, "test_cancel.json"),
		TotalSize:   500,
		TotalBlocks: 5,
		BlockStates: make([]int, 5),
		Clients:     make(map[string]chan struct{}),
		Starving:    make(map[string]int),
		NotifyChan:  make(chan struct{}, 10),
	}

	ctx, cancel := context.WithCancel(context.Background())
	reader := NewCacheReader(ctx, state, nil)

	// Trigger cancellation immediately
	cancel()

	buf := make([]byte, 10)
	n, err := reader.ReadAt(buf, 0)
	if n != 0 || err != context.Canceled {
		t.Errorf("Expected ReadAt to fail with context.Canceled, got n=%d, err=%v", n, err)
	}

	_ = reader.Close()
	if len(state.Clients) != 0 {
		t.Errorf("Expected clients list to be empty after Close, got %d", len(state.Clients))
	}
}

func TestOnDemandEviction(t *testing.T) {
	defer cleanupAfterTest(t, nil)

	// Clear registry size
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.TotalCacheSize = 0
	GlobalRegistry.Lock.Unlock()

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
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.TotalCacheSize = 40
	GlobalRegistry.Lock.Unlock()

	// Update Max Cache size limit to 25 bytes (triggers immediate eviction on next write/update)
	oldMax := GlobalConfig.CacheMaxBytes
	GlobalConfig.CacheMaxBytes = 25
	defer func() {
		GlobalConfig.CacheMaxBytes = oldMax
	}()

	// Trigger eviction via size update (asynchronous)
	GlobalRegistry.UpdateSize(5)

	// Wait for async eviction to complete (up to 1s)
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

func TestRevalidationChangeDetection(t *testing.T) {
	defer cleanupAfterTest(t, nil)

	// Set up mock HTTP upstream server representing the IPTV CDN
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

	// Write mock cache file and companion metadata JSON
	_ = os.WriteFile(filePath, make([]byte, 1000), 0644)
	meta := FileMeta{
		URL:           server.URL,
		TotalSize:     1000,
		ETag:          "etag-v1",
		LastValidated: time.Now().Add(-2 * time.Hour).Unix(), // Expired validation
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

	// 2. Modify upstream server to return different ETag (simulating updated media)
	mockETag = "etag-v2"
	// Set last validated time back to expired
	metaBytes, _ = os.ReadFile(metaPath)
	_ = json.Unmarshal(metaBytes, &meta)
	meta.LastValidated = time.Now().Add(-2 * time.Hour).Unix()
	metaBytes, _ = json.Marshal(meta)
	_ = os.WriteFile(metaPath, metaBytes, 0644)

	// Run revalidation. Should detect modification and evict cache.
	valid, err = Revalidate(key, server.URL, nil)
	if err != nil || valid {
		t.Errorf("Expected revalidation to detect modification and return valid=false, got valid=%t, err=%v", valid, err)
	}
	if FileExists(filePath) || FileExists(metaPath) {
		t.Errorf("Expected cached file and companion JSON to be evicted (deleted)")
	}
}

func TestDebouncedSchedulerTimeSlicing(t *testing.T) {
	// Spin up mock server serving data
	var connCount int
	var lock sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		connCount++
		lock.Unlock()
		w.Header().Set("Content-Range", "bytes 0-4/500")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()

	state := &FileCacheState{
		Key:         "test_sched",
		URL:         server.URL,
		Path:        filepath.Join(GlobalConfig.CacheDir, "test_sched"),
		PartPath:    filepath.Join(GlobalConfig.TmpDir, "test_sched.part"),
		MetaPath:    filepath.Join(GlobalConfig.TmpDir, "test_sched.json"),
		TotalSize:   500,
		TotalBlocks: 5,
		BlockStates: make([]int, 5),
		Clients:     make(map[string]chan struct{}),
		Starving:    make(map[string]int),
		NotifyChan:  make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	// Set debouncer to 0 to trigger instant preemption in single-worker setup
	oldMinBytes := GlobalConfig.SchedulerMinBytes
	oldMinTime := GlobalConfig.SchedulerMinTime
	GlobalConfig.SchedulerMinBytes = 0
	GlobalConfig.SchedulerMinTime = 0
	defer func() {
		GlobalConfig.SchedulerMinBytes = oldMinBytes
		GlobalConfig.SchedulerMinTime = oldMinTime
	}()

	// Trigger Coordinator
	StartCoordinator(state, nil)
	time.Sleep(20 * time.Millisecond) // Let it connect

	state.Lock.Lock()
	state.Starving["client-1"] = 3 // Client 1 wants block 3
	state.Lock.Unlock()
	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	// Wait for coordinator to evaluate starvation and jump
	time.Sleep(50 * time.Millisecond)

	lock.Lock()
	count := connCount
	lock.Unlock()

	// Connection count should be 2 (initial connection + jump connection)
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
		{"1.5g", 0, true}, // Invalid integer format
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
	// Spin up mock server serving data
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

	state := &FileCacheState{
		Key:         "test_loop",
		URL:         server.URL,
		Path:        filepath.Join(GlobalConfig.CacheDir, "test_loop"),
		PartPath:    filepath.Join(GlobalConfig.TmpDir, "test_loop.part"),
		MetaPath:    filepath.Join(GlobalConfig.TmpDir, "test_loop.json"),
		TotalSize:   500,
		TotalBlocks: 5,
		BlockStates: make([]int, 5),
		Clients:     make(map[string]chan struct{}),
		Starving:    make(map[string]int),
		NotifyChan:  make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	state.Lock.Lock()
	state.Clients["client-1"] = make(chan struct{}, 1)
	state.Clients["client-2"] = make(chan struct{}, 1)
	state.Starving["client-1"] = 0 // Client 1 wants block 0
	state.Starving["client-2"] = 3 // Client 2 wants block 3
	state.Lock.Unlock()

	// Trigger Coordinator
	StartCoordinator(state, nil)

	// Wait a bit to let it connect and run
	time.Sleep(50 * time.Millisecond)

	lock.Lock()
	count := connCount
	lock.Unlock()

	// Since client-1 is starving for block 0 (or client-2 for block 3),
	// the coordinator should NOT jump to the other starving block immediately.
	// Therefore, it should only have 1 connection instead of constantly jumping.
	if count > 1 {
		t.Errorf("Expected coordinator to stay on block and NOT jump immediately (connection count <= 1), got %d", count)
	}

	// Clean up
	state.Lock.Lock()
	close(state.Clients["client-1"])
	close(state.Clients["client-2"])
	state.Lock.Unlock()
}

func TestHeaderPropagation(t *testing.T) {
	// 1. Set up a target mock CDN server that records headers it receives
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

	// 2. Set up our FileCacheState pointing to the mock CDN server
	state := &FileCacheState{
		Key:         "test_headers",
		URL:         cdnServer.URL,
		Path:        filepath.Join(GlobalConfig.CacheDir, "test_headers"),
		PartPath:    filepath.Join(GlobalConfig.TmpDir, "test_headers.part"),
		MetaPath:    filepath.Join(GlobalConfig.TmpDir, "test_headers.json"),
		TotalSize:   500,
		TotalBlocks: 5,
		BlockStates: make([]int, 5),
		Clients:     make(map[string]chan struct{}),
		Starving:    make(map[string]int),
		NotifyChan:  make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	// 3. Create initial request headers simulating Chrome
	chromeHeaders := make(http.Header)
	chromeHeaders.Set("User-Agent", "Mozilla/5.0 Chrome/120.0.0.0")
	chromeHeaders.Set("Accept-Language", "en-US,en;q=0.9")
	chromeHeaders.Set("X-Custom-Chrome-Header", "ChromeTestVal")

	// 4. Start the Coordinator with the initial Chrome headers
	StartCoordinator(state, chromeHeaders)

	// Wait for the coordinator to connect to the CDN and write block 0
	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	headersList := receivedHeaders
	lock.Unlock()

	if len(headersList) == 0 {
		t.Fatal("Expected mock CDN to receive request headers, but receivedHeaders is empty")
	}

	// Assert on the first request's headers
	headers := headersList[0]

	// 5. Assert that Chrome headers were propagated correctly
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

	// Range header should be set by the coordinator, not match Chrome's Range
	rng := headers.Get("Range")
	if rng != "bytes=0-499" {
		t.Errorf("Expected Range header to be bytes=0-499, got %q", rng)
	}
}

func TestCacheFinalizationOrder(t *testing.T) {
	// Setup cache and tmp dirs
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()
	defer cleanupAfterTest(t, nil)

	// 1. Test case: client Close() occurs AFTER coordinator exits
	t.Run("client_close_after_coordinator_exit", func(t *testing.T) {
		// Reset registry
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_final_1.mkv"
		state := &FileCacheState{
			Key:          key,
			URL:          "http://dummy",
			Path:         filepath.Join(GlobalConfig.CacheDir, key),
			PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
			MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
			TotalSize:    100,
			TotalBlocks:  1,
			BlockStates:  []int{BlockCompleted}, // Already complete in memory
			Clients:      make(map[string]chan struct{}),
		}

		// Create mock part file
		_ = os.WriteFile(state.PartPath, []byte("dummy data"), 0644)

		// Create reader
		reader := NewCacheReader(context.Background(), state, nil)

		// Mock coordinator active state
		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile

		// Simulate coordinator exit: sets WriterActive=false, len(Clients) is 1, so it doesn't finalize
		state.WriterActive = false

		// Close reader: this should trigger finalization because len(Clients) becomes 0 and WriterActive is false
		err := reader.Close()
		if err != nil {
			t.Fatalf("Reader Close failed: %v", err)
		}

		// Verify finalization
		if !FileExists(state.Path) {
			t.Errorf("Expected final cache file %s to exist, but it does not", state.Path)
		}
		if FileExists(state.PartPath) {
			t.Errorf("Expected part file %s to be removed/renamed, but it still exists", state.PartPath)
		}

		// Verify metadata completed status
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
		if len(meta.BlockStates) != 0 {
			t.Errorf("Expected metadata BlockStates to be nil, got %v", meta.BlockStates)
		}
	})

	// 2. Test case: coordinator exits AFTER client Close()
	t.Run("coordinator_exit_after_client_close", func(t *testing.T) {
		// Reset registry
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_final_2.mkv"
		state := &FileCacheState{
			Key:          key,
			URL:          "http://dummy",
			Path:         filepath.Join(GlobalConfig.CacheDir, key),
			PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
			MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
			TotalSize:    100,
			TotalBlocks:  1,
			BlockStates:  []int{BlockCompleted}, // Already complete in memory
			Clients:      make(map[string]chan struct{}),
		}

		// Create mock part file
		_ = os.WriteFile(state.PartPath, []byte("dummy data"), 0644)

		// Create reader
		reader := NewCacheReader(context.Background(), state, nil)

		// Mock coordinator active state
		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile

		// Client Close: len(Clients) becomes 0, but WriterActive is true, so reader Close does not finalize
		err := reader.Close()
		if err != nil {
			t.Fatalf("Reader Close failed: %v", err)
		}

		if FileExists(state.Path) {
			t.Errorf("Expected final cache file %s NOT to exist yet, but it does", state.Path)
		}

		// Now simulate coordinator defer finalization check:
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

		// Verify finalization
		if !FileExists(state.Path) {
			t.Errorf("Expected final cache file %s to exist, but it does not", state.Path)
		}
		if FileExists(state.PartPath) {
			t.Errorf("Expected part file %s to be removed/renamed, but it still exists", state.PartPath)
		}

		// Verify metadata completed status
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
}

func TestURLSanitizationAndKeyGeneration(t *testing.T) {
	// Test cases of URLs with different query params that should produce the same key and extension
	url1 := "http://infinity.gives/series/wflx2/31916535/1334565.mkv?token=foo&expiry=123"
	url2 := "http://infinity.gives/series/wflx2/31916535/1334565.mkv?token=bar"
	url3 := "http://infinity.gives/series/wflx2/31916535/1334565.mkv"

	key1, ext1 := getCacheKeyAndExt(url1)
	key2, ext2 := getCacheKeyAndExt(url2)
	key3, ext3 := getCacheKeyAndExt(url3)

	if key1 != key2 || key2 != key3 {
		t.Errorf("Expected identical keys, got key1=%s, key2=%s, key3=%s", key1, key2, key3)
	}

	if ext1 != ".mkv" || ext2 != ".mkv" || ext3 != ".mkv" {
		t.Errorf("Expected extension to be '.mkv' and clean of query parameters, got ext1=%s, ext2=%s, ext3=%s", ext1, ext2, ext3)
	}

	// Test URL with no extension
	urlNoExt := "http://infinity.gives/series/wflx2/31916535/1334565?token=abc"
	keyNoExt, extNoExt := getCacheKeyAndExt(urlNoExt)
	if extNoExt != ".mkv" {
		t.Errorf("Expected default '.mkv' extension when none is provided in path, got: %s", extNoExt)
	}
	if keyNoExt == key1 {
		t.Errorf("Expected different key for different path, got same key %s", keyNoExt)
	}
}

func TestLegacyMigration(t *testing.T) {
	// Prepare legacy file (no extension) in test cache
	targetURL := "http://localhost/series/test_legacy.mkv"
	key, ext := getCacheKeyAndExt(targetURL)
	legacyPath := filepath.Join(GlobalConfig.CacheDir, key)
	newPath := filepath.Join(GlobalConfig.CacheDir, key+ext)

	// Write mock legacy file
	legacyData := []byte("legacy video bytes content")
	_ = os.WriteFile(legacyPath, legacyData, 0644)
	defer os.Remove(legacyPath)
	defer os.Remove(newPath)
	defer os.Remove(newPath + ".json")

	// Run migration helper
	err := migrateLegacyFile(legacyPath, newPath, targetURL)
	if err != nil {
		t.Fatalf("Failed to run migrateLegacyFile: %v", err)
	}

	// Verify legacy file is moved/deleted
	if FileExists(legacyPath) {
		t.Errorf("Expected legacy file at %s to be moved/deleted, but it exists", legacyPath)
	}

	// Verify new file exists
	if !FileExists(newPath) {
		t.Errorf("Expected new file at %s to exist, but it does not", newPath)
	}

	// Verify metadata JSON exists and matches
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
	// Prepare test file
	targetURL := "http://localhost/series/test_reval.mkv"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext
	filePath := filepath.Join(GlobalConfig.CacheDir, cacheFileName)
	metaPath := filePath + ".json"

	// Ensure no leftover files
	_ = os.Remove(filePath)
	_ = os.Remove(metaPath)
	defer os.Remove(filePath)
	defer os.Remove(metaPath)

	// Write mock cache file
	fileData := []byte("some cache video bytes")
	err := os.WriteFile(filePath, fileData, 0644)
	if err != nil {
		t.Fatalf("Failed to write mock cache file: %v", err)
	}

	// Verify metadata file does not exist initially
	if FileExists(metaPath) {
		t.Fatalf("Expected metadata file %s to not exist", metaPath)
	}

	// Save current revalidate interval and set it to a large value for this test
	oldInterval := GlobalConfig.CacheRevalidateInterval
	GlobalConfig.CacheRevalidateInterval = 1 * time.Hour
	defer func() {
		GlobalConfig.CacheRevalidateInterval = oldInterval
	}()

	// Run Revalidate. Since the metadata file is missing, it should recreate it dynamically.
	// Since the recreation sets LastValidated to the file's ModTime (which is practically now),
	// it will be within the 1-hour revalidation window, so it won't perform an HTTP probe.
	valid, err := Revalidate(cacheFileName, targetURL, nil)
	if err != nil {
		t.Fatalf("Revalidate returned error: %v", err)
	}
	if !valid {
		t.Errorf("Expected Revalidate to return true, got false")
	}

	// Verify metadata JSON has been recreated
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
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()
	defer cleanupAfterTest(t, nil)

	t.Run("incomplete_coordinator_exit_with_active_client", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		key := "test_incomplete_client.mkv"
		state := &FileCacheState{
			Key:          key,
			URL:          "http://dummy",
			Path:         filepath.Join(GlobalConfig.CacheDir, key),
			PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
			MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
			TotalSize:    100,
			TotalBlocks:  2,
			BlockStates:  []int{BlockCompleted, BlockNotStarted}, // Incomplete
			Clients:      make(map[string]chan struct{}),
		}

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		_ = os.WriteFile(state.PartPath, []byte("partial data"), 0644)
		_ = os.WriteFile(state.MetaPath, []byte("{}"), 0644)

		// Create client reader
		reader := NewCacheReader(context.Background(), state, nil)

		// Mock active coordinator exiting
		state.Lock.Lock()
		state.WriterActive = true
		mockFile, _ := os.OpenFile(state.PartPath, os.O_RDWR, 0644)
		state.File = mockFile
		state.Lock.Unlock()

		// Simulate coordinator exit: sets WriterActive=false, evaluates isCompleted=false
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

		// Since we have an active client, it should NOT clean up yet
		if shouldCleanUpIncomplete {
			t.Errorf("Expected shouldCleanUpIncomplete to be false because client is active")
		}

		// Verify files and registry entry still exist
		if !FileExists(state.PartPath) {
			t.Errorf("Expected part file to exist while client is active")
		}

		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()
		if !inRegistry {
			t.Errorf("Expected file state to remain in registry while client is active")
		}

		// Now close the client reader. This should trigger the cleanup.
		err := reader.Close()
		if err != nil {
			t.Fatalf("Reader Close failed: %v", err)
		}

		// Verify everything is deleted/cleaned up
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
		state := &FileCacheState{
			Key:          key,
			URL:          "http://dummy",
			Path:         filepath.Join(GlobalConfig.CacheDir, key),
			PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
			MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
			TotalSize:    100,
			TotalBlocks:  2,
			BlockStates:  []int{BlockCompleted, BlockNotStarted}, // Incomplete
			Clients:      make(map[string]chan struct{}),
		}

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

		// Simulate coordinator exit: sets WriterActive=false, evaluates isCompleted=false
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

		// Verify everything is deleted/cleaned up immediately on coordinator exit
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
	// Setup cache and tmp dirs
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()
	defer cleanupAfterTest(t, nil)

	// Mock coordinator sleep to avoid actual delay in test execution
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

		// Server returns 403 Forbidden
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()

		key := "test_403_fast_exit.mkv"
		state := &FileCacheState{
			Key:          key,
			URL:          server.URL,
			Path:         filepath.Join(GlobalConfig.CacheDir, key),
			PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
			MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
			TotalSize:    100,
			TotalBlocks:  2,
			BlockStates:  []int{BlockNotStarted, BlockNotStarted},
			Clients:      make(map[string]chan struct{}),
			Starving:     make(map[string]int),
			NotifyChan:   make(chan struct{}, 10),
		}

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		// Start coordinator
		StartCoordinator(state, nil)

		// Wait for coordinator to run and exit (should exit immediately on 403)
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

		// Since clients count is 0, the registry entry should be deleted on exit
		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()

		if inRegistry {
			t.Errorf("Expected registry entry to be cleaned up on coordinator exit")
		}
	})

	t.Run("consecutive_failures_exhaustion_exit", func(t *testing.T) {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = make(map[string]*FileCacheState)
		GlobalRegistry.Lock.Unlock()

		// Server returns 500 Internal Server Error
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer server.Close()

		key := "test_500_exhaustion.mkv"
		state := &FileCacheState{
			Key:          key,
			URL:          server.URL,
			Path:         filepath.Join(GlobalConfig.CacheDir, key),
			PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
			MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
			TotalSize:    100,
			TotalBlocks:  2,
			BlockStates:  []int{BlockNotStarted, BlockNotStarted},
			Clients:      make(map[string]chan struct{}),
			Starving:     make(map[string]int),
			NotifyChan:   make(chan struct{}, 10),
		}

		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files[key] = state
		GlobalRegistry.Lock.Unlock()

		// Start coordinator
		StartCoordinator(state, nil)

		// Wait for coordinator to run, fail 8 times (with mocked 0s sleep), and exit
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

		// Since clients count is 0, the registry entry should be deleted on exit
		GlobalRegistry.Lock.Lock()
		_, inRegistry := GlobalRegistry.Files[key]
		GlobalRegistry.Lock.Unlock()

		if inRegistry {
			t.Errorf("Expected registry entry to be cleaned up after failure exit")
		}
	})
}

func TestCoordinatorNoBlockSkippingOnFailure(t *testing.T) {
	// Setup cache and tmp dirs
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	// Mock coordinator sleep to avoid actual delay in test execution
	coordinatorSleepMu.Lock()
	oldSleep := coordinatorSleepFn
	coordinatorSleepFn = func(d time.Duration) {}
	coordinatorSleepMu.Unlock()
	defer func() {
		coordinatorSleepMu.Lock()
		coordinatorSleepFn = oldSleep
		coordinatorSleepMu.Unlock()
	}()

	// Mock server that returns a 500 on the very first request, and then succeeds
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
	state := &FileCacheState{
		Key:          key,
		URL:          server.URL,
		Path:         filepath.Join(GlobalConfig.CacheDir, key),
		PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
		MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
		TotalSize:    200,
		TotalBlocks:  2,
		BlockStates:  []int{BlockNotStarted, BlockNotStarted},
		Clients:      make(map[string]chan struct{}),
		Starving:     make(map[string]int),
		NotifyChan:   make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// Start coordinator
	StartCoordinator(state, nil)

	// Wait for coordinator to run, fail once, and reconnect
	time.Sleep(100 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	// We expect at least two requests:
	// First request: bytes=0-199
	// Second request: bytes=0-199 (NOT bytes=100-199, which would skip block 0!)
	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 requests, got %d", len(ranges))
	}

	if ranges[0] != "bytes=0-199" {
		t.Errorf("Expected first request range to be bytes=0-199, got %s", ranges[0])
	}
	if ranges[1] != "bytes=0-199" {
		t.Errorf("Expected second request range to be bytes=0-199 (retrying block 0), got %s (indicates block skipping!)", ranges[1])
	}
}

func cleanupAfterTest(t *testing.T, state *FileCacheState) {
	// If a coordinator was started for this state, wait for it to exit
	if state != nil {
		// Wait up to 1 second for coordinator to exit
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

	// Wait up to 1 second for eviction to finish
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

	// Clean up registry
	GlobalRegistry.Lock.Lock()
	if state != nil {
		delete(GlobalRegistry.Files, state.Key)
	}
	// Also clear registry of other test files to be safe
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Lock.Unlock()
}

func TestRobustProbe(t *testing.T) {
	// 1. Test GetCleanHost and CopySafeHeaders
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

	// 2. Test Probe Fallback behavior
	// Mock server that returns 403 Forbidden for Range requests, but returns 200 OK for standard requests.
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

	// Perform the probe
	ctx := context.Background()
	size, etag, lm, notModified, err := probeUpstream(ctx, server.URL, nil, nil)
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

	// Check that learning cache recorded standard probe preferred method
	host := GetCleanHost(server.URL)
	if pref := GetProbeMethod(host); pref != ProbeMethodStandard {
		t.Errorf("Expected preferred method to be standard, got %s", pref)
	}

	// Make another probe request to the same server. Since standard GET is preferred now,
	// it should go straight to standard probe and NOT send any range requests this time.
	lock.Lock()
	requestCountBefore := requestCount
	lock.Unlock()

	size2, _, _, _, err := probeUpstream(ctx, server.URL, nil, nil)
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
	// Set up directories
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

	// Save and mock config
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	oldBlockSize := GlobalConfig.BlockSize
	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.ClientHydrationWait = 0 // Disable wait to trigger instant spawn for seek
	GlobalConfig.BlockSize = 100         // Block size 100 bytes

	defer func() {
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
		GlobalConfig.BlockSize = oldBlockSize
	}()

	// Mock server
	var lock sync.Mutex
	var requestedRanges []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		lock.Unlock()

		w.Header().Set("Content-Range", "bytes 0-99/400") // total size 400
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer server.Close()

	key := "test_concurrent.mkv"
	state := &FileCacheState{
		Key:          key,
		URL:          server.URL,
		Path:         filepath.Join(GlobalConfig.CacheDir, key),
		PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
		MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
		TotalSize:    400,
		TotalBlocks:  4, // 4 blocks of size 100
		BlockStates:  []int{BlockNotStarted, BlockNotStarted, BlockNotStarted, BlockNotStarted},
		Clients:      make(map[string]chan struct{}),
		Starving:     make(map[string]int),
		NotifyChan:   make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// 1. Start coordinator. It will automatically spawn worker 1 starting at block 0.
	StartCoordinator(state, nil)
	time.Sleep(50 * time.Millisecond)

	// 2. Simulate client starvation for block 3 (seek to block 3)
	state.Lock.Lock()
	state.Starving["client-test"] = 3
	state.Lock.Unlock()

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	// Wait for coordinator to detect starvation and spawn worker 2
	time.Sleep(100 * time.Millisecond)

	// Verify that multiple ranges were requested concurrently
	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 range requests, got %d. Ranges: %v", len(ranges), ranges)
	}

	// We expect one worker to request from block 0 (bytes=0-399) and another from block 3 (bytes=300-399)
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
		t.Errorf("Expected requests starting at block 0 (bytes=0-399) and block 3 (bytes=300-399). Got: %v", ranges)
	}
}

func TestClientSequentialReadSuppression(t *testing.T) {
	// Setup cache and tmp dirs
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	tmpDir := t.TempDir()
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	// Save and mock config
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	oldBlockSize := GlobalConfig.BlockSize
	GlobalConfig.MaxUpstreamWorkers = 2
	GlobalConfig.ClientHydrationWait = 2 * time.Second
	GlobalConfig.BlockSize = 100

	defer func() {
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
		GlobalConfig.BlockSize = oldBlockSize
	}()

	// Mock server with delay for block 0 to keep Worker 1 active
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
	state := &FileCacheState{
		Key:          key,
		URL:          server.URL,
		Path:         filepath.Join(GlobalConfig.CacheDir, key),
		PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
		MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
		TotalSize:    400,
		TotalBlocks:  4,
		BlockStates:  []int{BlockNotStarted, BlockNotStarted, BlockNotStarted, BlockNotStarted},
		Clients:      make(map[string]chan struct{}),
		Starving:     make(map[string]int),
		NotifyChan:   make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// 1. Start coordinator. It will automatically spawn worker 1 starting at block 0.
	StartCoordinator(state, nil)
	time.Sleep(50 * time.Millisecond)

	// 2. Simulate sequential client reading block 1 (sb = 1, worker currentBlock = 0)
	state.Lock.Lock()
	state.Starving["client-test"] = 1
	state.Lock.Unlock()

	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	// Wait for coordinator to evaluate starvation
	time.Sleep(100 * time.Millisecond)

	// Verify that ONLY 1 upstream connection range request was made (no second worker spawned for block 1)
	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	if len(ranges) != 1 {
		t.Errorf("Expected exactly 1 range request (suppression should prevent worker 2 from spawning for block 1), but got %d. Ranges: %v", len(ranges), ranges)
	}
}

func TestConnectionTelemetry(t *testing.T) {
	state := &FileCacheState{}
	
	// Start connection
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

	// Start a second connection
	conn2 := "conn-2"
	state.RecordConnStart(conn2)
	if len(state.RecentConnections) != 2 {
		t.Errorf("Expected 2 recent connections, got %d", len(state.RecentConnections))
	}

	// Close the first connection
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
	// Set up directories
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

	// Save and mock config
	oldMaxWorkers := GlobalConfig.MaxUpstreamWorkers
	oldWait := GlobalConfig.ClientHydrationWait
	oldBlockSize := GlobalConfig.BlockSize
	GlobalConfig.MaxUpstreamWorkers = 1
	GlobalConfig.ClientHydrationWait = 0
	GlobalConfig.BlockSize = 100 // Block size 100 bytes

	defer func() {
		GlobalConfig.MaxUpstreamWorkers = oldMaxWorkers
		GlobalConfig.ClientHydrationWait = oldWait
		GlobalConfig.BlockSize = oldBlockSize
	}()

	// Mock server
	var lock sync.Mutex
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		reqCount++
		currentReq := reqCount
		lock.Unlock()

		if currentReq == 1 {
			// First request: serve block 0, but only 100 bytes (EOF next)
			w.Header().Set("Content-Range", "bytes 0-99/200")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
		} else if currentReq == 2 {
			// Second request: return failure
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			// Subsequent requests: serve block 1
			w.Header().Set("Content-Range", "bytes 100-199/200")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(make([]byte, 100))
		}
	}))
	defer server.Close()

	key := "test_cooldown.mkv"
	state := &FileCacheState{
		Key:          key,
		URL:          server.URL,
		Path:         filepath.Join(GlobalConfig.CacheDir, key),
		PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
		MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
		TotalSize:    200,
		TotalBlocks:  2, // 2 blocks of size 100
		BlockStates:  []int{BlockNotStarted, BlockNotStarted},
		Clients:      make(map[string]chan struct{}),
		Starving:     make(map[string]int),
		NotifyChan:   make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// Start coordinator. It will automatically spawn worker 1 starting at block 0.
	StartCoordinator(state, nil)

	// Wait for the worker to finish both blocks (and retry after error)
	// Retry backoff starts at 2 seconds.
	time.Sleep(3 * time.Second)

	state.Lock.Lock()
	cooldown := state.ObservedCooldown
	block0Completed := state.BlockStates[0] == BlockCompleted
	block1Completed := state.BlockStates[1] == BlockCompleted
	state.Lock.Unlock()

	if !block0Completed || !block1Completed {
		t.Errorf("Expected both blocks to complete, got: block0=%v, block1=%v", block0Completed, block1Completed)
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

	_, _, _, _, err := probeUpstream(context.Background(), server.URL, nil, nil)
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
		w.WriteHeader(http.StatusOK) // 200 but HTML content type
	}))
	defer server.Close()

	_, _, _, _, err := probeUpstream(context.Background(), server.URL, nil, nil)
	if err == nil {
		t.Fatal("Expected probe to fail")
	}

	lock.Lock()
	count := reqCount
	lock.Unlock()

	// Should abort immediately after Range probe (exactly 1 request)
	// and NOT execute standard GET fallback
	if count != 1 {
		t.Errorf("Expected exactly 1 request to mock server, got %d", count)
	}
}

func TestProbeFailureNegativeCaching(t *testing.T) {
	// Initialize registry
	InitRegistry()

	var lock sync.Mutex
	reqCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		reqCount++
		lock.Unlock()

		w.WriteHeader(http.StatusNotFound) // 404 Not Found (Permanent & Catastrophic)
	}))
	defer server.Close()

	// Save and set configs
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

	// First request: Cache MISS, should hit mock server and fail
	req := httptest.NewRequest("GET", "http://localhost:8080/play/"+key, nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if GlobalRegistry.IsProbeFailed(key) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("Gateway Bypass"))
			return
		}

		_, _, _, _, err := probeUpstream(r.Context(), targetURL, r.Header, nil)
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

	// Request 1: should call probe and fail (reqCount = 1)
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

	// Request 2: within cooldown (200ms), should bypass probe and serve 502 immediately
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

	// Wait for cooldown to expire
	time.Sleep(250 * time.Millisecond)

	// Request 3: cooldown expired, should hit server again (reqCount = 2)
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
	// Setup cache and tmp dirs
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	GlobalConfig.CacheDir = t.TempDir()
	GlobalConfig.TmpDir = t.TempDir()
	defer func() {
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
	}()

	// Temporarily override BlockSize and SchedulerMinBytes
	oldBlockSize := GlobalConfig.BlockSize
	oldMinBytes := GlobalConfig.SchedulerMinBytes
	GlobalConfig.BlockSize = 100
	GlobalConfig.SchedulerMinBytes = 200 // Grab 2 blocks (200 bytes) at the end
	defer func() {
		GlobalConfig.BlockSize = oldBlockSize
		GlobalConfig.SchedulerMinBytes = oldMinBytes
	}()

	var lock sync.Mutex
	var requestedRanges []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lock.Lock()
		requestedRanges = append(requestedRanges, r.Header.Get("Range"))
		lock.Unlock()

		w.Header().Set("Content-Range", "bytes 0-999/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 1000))
	}))
	defer server.Close()

	key := "test_prefetch.mkv"
	state := &FileCacheState{
		Key:          key,
		URL:          server.URL,
		Path:         filepath.Join(GlobalConfig.CacheDir, key),
		PartPath:     filepath.Join(GlobalConfig.TmpDir, key+".part"),
		MetaPath:     filepath.Join(GlobalConfig.CacheDir, key+".json"),
		TotalSize:    1000,
		TotalBlocks:  10, // 10 blocks of 100 bytes
		BlockStates:  make([]int, 10),
		Clients:      make(map[string]chan struct{}),
		Starving:     make(map[string]int),
		NotifyChan:   make(chan struct{}, 10),
	}
	defer cleanupAfterTest(t, state)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// Simulate client starving on block 0
	state.Lock.Lock()
	state.Starving["client-1"] = 0
	state.Lock.Unlock()

	// Start coordinator. It will spawn Worker 1 starting at starving block 0.
	StartCoordinator(state, nil)

	// Sleep briefly to let the worker start pre-fetching
	time.Sleep(100 * time.Millisecond)

	// Simulate player requesting block 9 (last block) concurrently to simulate anti-sharing/starvation storm
	state.Lock.Lock()
	state.Starving["client-2"] = 9
	state.Lock.Unlock()
	select {
	case state.NotifyChan <- struct{}{}:
	default:
	}

	// Wait for coordinator to run and process the notification
	time.Sleep(150 * time.Millisecond)

	lock.Lock()
	ranges := make([]string, len(requestedRanges))
	copy(ranges, requestedRanges)
	lock.Unlock()

	// Verify requested ranges
	if len(ranges) < 2 {
		t.Fatalf("Expected at least 2 requests (1 prefetch, 1 sequential), got %d", len(ranges))
	}

	// The first request must be the last 2 blocks (blocks 8 and 9) range: bytes=800-999
	expectedPrefetchRange := "bytes=800-999"
	if ranges[0] != expectedPrefetchRange {
		t.Errorf("Expected first request range (prefetch) to be %s, got %s", expectedPrefetchRange, ranges[0])
	}

	// The second request should be block 0 range starting from 0: bytes=0-999
	expectedSequentialRange := "bytes=0-999"
	if ranges[1] != expectedSequentialRange {
		t.Errorf("Expected second request range to be %s, got %s", expectedSequentialRange, ranges[1])
	}
}

func TestCommittingStateIncluded(t *testing.T) {
	// Initialize / clear registry
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
	state := &FileCacheState{
		Key:            key,
		URL:            "http://example.com/test.mkv",
		BlockStates:    []int{BlockCompleted, BlockCompleted},
		UpstreamStatus: "Committing",
	}

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = state
	GlobalRegistry.Lock.Unlock()

	// Perform a mock HTTP request to /debug/state
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

