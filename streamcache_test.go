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




