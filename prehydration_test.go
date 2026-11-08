package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPrehydrationCapacityThresholds(t *testing.T) {
	oldRes := GlobalConfig.GetPrehydrateCapacityReservation()
	oldPrefetch := GlobalConfig.GetEnableEpisodePrehydration()
	GlobalConfig.SetPrehydrateCapacityReservation(1)
	GlobalConfig.SetEnableEpisodePrehydration(true)
	defer func() {
		GlobalConfig.SetPrehydrateCapacityReservation(oldRes)
		GlobalConfig.SetEnableEpisodePrehydration(oldPrefetch)
	}()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}

	// 1. Report shows max=3, active=1, available=2. Reservation=1.
	// Can start? available (2) >= reservation (1) + 1 -> True!
	pm.latestCapacity = &ProviderCapacityReport{
		Status:                 "ok",
		TotalMaxStreams:        3,
		TotalActiveStreams:     1,
		TotalAvailableStreams:  2,
		TargetAvailableStreams: 2,
	}
	if !pm.CanStartPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep2") {
		t.Fatalf("Expected CanStartPrehydration=true when available=2 and reservation=1")
	}
	if pm.ShouldYieldPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep2") {
		t.Fatalf("Expected ShouldYieldPrehydration=false when available=2 and reservation=1")
	}

	// 2. Prefetch started: now active=2, available=1.
	// Can start another? available (1) >= reservation (1) + 1 -> False!
	// Should yield? running prefetch is already in active, so available (1) < reservation (1) is False!
	pm.latestCapacity = &ProviderCapacityReport{
		Status:                 "ok",
		TotalMaxStreams:        3,
		TotalActiveStreams:     2,
		TotalAvailableStreams:  1,
		TargetAvailableStreams: 1,
	}
	if pm.CanStartPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep3") {
		t.Fatalf("Expected CanStartPrehydration=false when available=1 and reservation=1")
	}
	if pm.ShouldYieldPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep2") {
		t.Fatalf("Expected ShouldYieldPrehydration=false when available=1 and reservation=1 (self-accounting fix)")
	}

	// 3. Live stream started elsewhere: now active=3, available=0.
	// Should yield? available (0) < reservation (1) -> True!
	pm.latestCapacity = &ProviderCapacityReport{
		Status:                 "ok",
		TotalMaxStreams:        3,
		TotalActiveStreams:     3,
		TotalAvailableStreams:  0,
		TargetAvailableStreams: 0,
	}
	if !pm.ShouldYieldPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep2") {
		t.Fatalf("Expected ShouldYieldPrehydration=true when available=0 < reservation=1")
	}

	// 4. Stasharr capacity unavailable / unconfigured fallback mode:
	pm.latestCapacity = &ProviderCapacityReport{
		Status: "unavailable",
	}
	// With 0 active prehydrations:
	if !pm.CanStartPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep2") {
		t.Fatalf("Expected CanStartPrehydration=true in unmetered mode when 0 active prehydrations")
	}
	// With 1 active prehydration:
	pm.activePrehydrations["some-key"] = true
	if pm.CanStartPrehydration("http://dispatcharr:9191/proxy/vod/episode/ep3") {
		t.Fatalf("Expected CanStartPrehydration=false in unmetered mode when 1 active prehydration already running")
	}
}

func TestYieldAndPriorityUpgrade(t *testing.T) {
	tmpDir := t.TempDir()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}

	state := NewFileCacheState(
		"test-ep2.mkv",
		"http://dispatcharr:9191/proxy/vod/episode/ep2",
		filepath.Join(tmpDir, "test-ep2.mkv"),
		filepath.Join(tmpDir, "test-ep2.mkv.part"),
		filepath.Join(tmpDir, "test-ep2.mkv.json"),
		"video/mp4",
		"",
		"",
		10*1024*1024,
	)
	state.IsPrehydrating = true
	state.RangeSet.Add(0, 1024*1024) // 1MB downloaded

	canceled := false
	_, cancel := context.WithCancel(context.Background())
	state.Cancel = func() {
		canceled = true
		cancel()
	}
	state.WriterActive = true
	pm.activePrehydrations[state.Key] = true

	// 1. Yield state
	pm.YieldState(state, "Test capacity encroachment")

	if !canceled {
		t.Fatalf("Expected worker context to be cancelled on Yield")
	}
	if !state.IsYielding {
		t.Fatalf("Expected state.IsYielding=true after YieldState")
	}
	if state.WriterActive {
		t.Fatalf("Expected state.WriterActive=false after YieldState")
	}
	if state.UpstreamStatus != "Yielding" {
		t.Fatalf("Expected state.UpstreamStatus='Yielding', got %s", state.UpstreamStatus)
	}
	if pm.activePrehydrations[state.Key] {
		t.Fatalf("Expected state to be removed from activePrehydrations on Yield")
	}
	// Verify .part data was preserved
	if state.RangeSet.TotalBytes() != 1024*1024 {
		t.Fatalf("Expected RangeSet data to be preserved (1MB), got %d", state.RangeSet.TotalBytes())
	}

	// 2. Priority Upgrade (real client connects to stream)
	state.Lock.Lock()
	wasPrehydrating := state.IsPrehydrating || state.IsYielding
	state.IsPrehydrating = false
	state.IsYielding = false
	state.Lock.Unlock()

	if !wasPrehydrating {
		t.Fatalf("Expected wasPrehydrating=true")
	}
	if state.IsPrehydrating || state.IsYielding {
		t.Fatalf("Expected IsPrehydrating and IsYielding to be false after live client upgrade")
	}
}

func TestDownshiftOnDisconnect(t *testing.T) {
	tmpDir := t.TempDir()

	oldEvict := GlobalConfig.GetEvictYieldedParts()
	GlobalConfig.SetEvictYieldedParts(false)
	defer GlobalConfig.SetEvictYieldedParts(oldEvict)

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		latestCapacity: &ProviderCapacityReport{
			Status:                 "ok",
			TotalMaxStreams:        2,
			TotalActiveStreams:     1,
			TotalAvailableStreams:  1, // At reservation limit -> will yield on downshift
			TargetAvailableStreams: 1,
		},
	}

	partPath := filepath.Join(tmpDir, "movie.mkv.part")
	_ = os.WriteFile(partPath, []byte("partial-content"), 0644)

	state := NewFileCacheState(
		"movie.mkv",
		"http://dispatcharr:9191/proxy/vod/movie/123",
		filepath.Join(tmpDir, "movie.mkv"),
		partPath,
		filepath.Join(tmpDir, "movie.mkv.json"),
		"video/mp4",
		"",
		"",
		100*1024*1024,
	)
	state.IsPrehydrating = false
	state.RangeSet.Add(0, 50*1024*1024)

	// Simulate client disconnect downshift
	pm.OnDownshift(state)

	if !state.IsPrehydrating {
		t.Fatalf("Expected state.IsPrehydrating=true after client disconnect")
	}
	if !state.IsYielding {
		t.Fatalf("Expected state.IsYielding=true because available capacity is at reservation limit")
	}
	// Verify .part file on disk was NOT deleted
	if !FileExists(partPath) {
		t.Fatalf("Expected .part file to be preserved on disconnect downshift")
	}
}

func TestCompletionGatedChaining(t *testing.T) {
	tmpDir := t.TempDir()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}

	state := NewFileCacheState(
		"ep1.mkv",
		"http://dispatcharr:9191/proxy/vod/episode/ep1",
		filepath.Join(tmpDir, "ep1.mkv"),
		filepath.Join(tmpDir, "ep1.mkv.part"),
		filepath.Join(tmpDir, "ep1.mkv.json"),
		"video/mp4",
		"",
		"",
		1024,
	)
	state.NextURLs = []string{
		"http://dispatcharr:9191/proxy/vod/episode/ep2",
		"http://dispatcharr:9191/proxy/vod/episode/ep3",
	}

	// Trigger completion
	pm.OnHydrationCompleted(state)

	// Verify ep2 was enqueued with ep3 in its next chain
	pm.mu.Lock()
	if len(pm.queue) != 1 {
		pm.mu.Unlock()
		t.Fatalf("Expected 1 task in queue, got %d", len(pm.queue))
	}
	task := pm.queue[0]
	pm.mu.Unlock()

	if task.TargetURL != "http://dispatcharr:9191/proxy/vod/episode/ep2" {
		t.Fatalf("Expected task URL ep2, got %s", task.TargetURL)
	}
	if len(task.NextURLs) != 1 || task.NextURLs[0] != "http://dispatcharr:9191/proxy/vod/episode/ep3" {
		t.Fatalf("Expected next chain to contain ep3, got %v", task.NextURLs)
	}
}

func TestStasharrCapacityPollIntegration(t *testing.T) {
	// Create mock Stasharr HTTP server
	mockStasharr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/plugins/stasharr/provider-capacity" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"status": "ok",
			"timestamp": %d,
			"total_max_streams": 4,
			"total_active_streams": 1,
			"total_available_streams": 3,
			"target_available_streams": 3,
			"providers": [
				{
					"account_id": 1,
					"account_name": "Test M3U",
					"max_streams": 4,
					"active_streams": 1,
					"available_streams": 3
				}
			]
		}`, time.Now().Unix())
	}))
	defer mockStasharr.Close()

	oldURL := GlobalConfig.GetDispatcharrURL()
	GlobalConfig.SetDispatcharrURL(mockStasharr.URL)
	defer GlobalConfig.SetDispatcharrURL(oldURL)

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}

	pm.pollCapacityOnce()

	rep := pm.GetLatestCapacity()
	if rep == nil || rep.Status != "ok" {
		t.Fatalf("Expected capacity report status 'ok', got %v", rep)
	}
	if rep.TotalMaxStreams != 4 || rep.TotalActiveStreams != 1 || rep.TotalAvailableStreams != 3 {
		t.Fatalf("Unexpected capacity numbers: %+v", rep)
	}
}

func TestPrehydrationConfigEnv(t *testing.T) {
	// 1. Defaults
	cfgDefault := parseConfigFromEnv()
	if cfgDefault.GetPrehydrateCapacityReservation() != 1 {
		t.Fatalf("Expected default capacity reservation 1, got %d", cfgDefault.GetPrehydrateCapacityReservation())
	}
	if cfgDefault.GetPrehydrateYieldBackoff() != 30*time.Second {
		t.Fatalf("Expected default yield backoff 30s, got %v", cfgDefault.GetPrehydrateYieldBackoff())
	}
	if cfgDefault.GetEnableEpisodePrehydration() != true {
		t.Fatalf("Expected default enable prehydration true, got %v", cfgDefault.GetEnableEpisodePrehydration())
	}

	// 2. Custom environment variables
	os.Setenv("PREHYDRATE_CAPACITY_RESERVATION", "2")
	os.Setenv("PREHYDRATE_YIELD_BACKOFF", "15s")
	os.Setenv("ENABLE_EPISODE_PREHYDRATION", "false")
	cfgCustom := parseConfigFromEnv()

	if cfgCustom.GetPrehydrateCapacityReservation() != 2 {
		t.Fatalf("Expected custom capacity reservation 2, got %d", cfgCustom.GetPrehydrateCapacityReservation())
	}
	if cfgCustom.GetPrehydrateYieldBackoff() != 15*time.Second {
		t.Fatalf("Expected custom yield backoff 15s, got %v", cfgCustom.GetPrehydrateYieldBackoff())
	}
	if cfgCustom.GetEnableEpisodePrehydration() != false {
		t.Fatalf("Expected custom enable prehydration false, got %v", cfgCustom.GetEnableEpisodePrehydration())
	}

	// Clean up
	os.Unsetenv("PREHYDRATE_CAPACITY_RESERVATION")
	os.Unsetenv("PREHYDRATE_YIELD_BACKOFF")
	os.Unsetenv("ENABLE_EPISODE_PREHYDRATION")
}

func TestServerMediaStreamNextURL(t *testing.T) {
	oldPM := GlobalPrehydrationManager
	defer func() { GlobalPrehydrationManager = oldPM }()

	mockPM := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}
	GlobalPrehydrationManager = mockPM

	// 1. Test Cache Hit triggers OnCacheHit and enqueues next_url
	testURL := "http://example.com/show/s01e01.mkv"
	key, ext := getCacheKeyAndExt(testURL)
	cacheFileName := key + ext
	cacheDir := GlobalConfig.GetCacheDir()
	filePath := filepath.Join(cacheDir, cacheFileName)

	testContent := []byte("fake video content for s01e01")
	if err := os.WriteFile(filePath, testContent, 0644); err != nil {
		t.Fatalf("Failed to write mock cache file: %v", err)
	}
	defer os.Remove(filePath)

	req := httptest.NewRequest("GET", fmt.Sprintf("/stream?url=%s&next_url=%s&next_url=%s",
		testURL, "http://example.com/show/s01e02.mkv", "http://example.com/show/s01e03.mkv"), nil)
	rec := httptest.NewRecorder()

	handleMediaStream(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for cache hit, got %d", rec.Code)
	}
	if rec.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("Expected X-Cache: HIT, got %s", rec.Header().Get("X-Cache"))
	}

	mockPM.mu.Lock()
	if len(mockPM.queue) != 1 {
		mockPM.mu.Unlock()
		t.Fatalf("Expected 1 prehydration task queued on cache hit, got %d", len(mockPM.queue))
	}
	task := mockPM.queue[0]
	mockPM.mu.Unlock()

	if task.TargetURL != "http://example.com/show/s01e02.mkv" {
		t.Errorf("Expected queued task URL ep2, got %s", task.TargetURL)
	}
	if len(task.NextURLs) != 1 || task.NextURLs[0] != "http://example.com/show/s01e03.mkv" {
		t.Errorf("Expected queued task nextUrls to contain ep3, got %v", task.NextURLs)
	}

	// 2. Test Comma-separated next_url query parsing
	mockPM.mu.Lock()
	mockPM.queue = nil
	mockPM.mu.Unlock()

	reqComma := httptest.NewRequest("GET", fmt.Sprintf("/stream?url=%s&next_url=%s",
		testURL, "http://example.com/show/s01e02.mkv,http://example.com/show/s01e03.mkv"), nil)
	recComma := httptest.NewRecorder()

	handleMediaStream(recComma, reqComma)

	mockPM.mu.Lock()
	if len(mockPM.queue) != 1 {
		mockPM.mu.Unlock()
		t.Fatalf("Expected 1 prehydration task queued for comma-separated next_url, got %d", len(mockPM.queue))
	}
	taskComma := mockPM.queue[0]
	mockPM.mu.Unlock()

	if taskComma.TargetURL != "http://example.com/show/s01e02.mkv" {
		t.Errorf("Expected queued task URL ep2, got %s", taskComma.TargetURL)
	}
	if len(taskComma.NextURLs) != 1 || taskComma.NextURLs[0] != "http://example.com/show/s01e03.mkv" {
		t.Errorf("Expected queued task nextUrls to contain ep3, got %v", taskComma.NextURLs)
	}
}

func TestPrehydrationSpawnWorkerNoDeadlock(t *testing.T) {
	// Create mock upstream HTTP server
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1024))
	}))
	defer mockUpstream.Close()

	tmpDir := t.TempDir()
	partPath := filepath.Join(tmpDir, "prehydrate_deadlock_test.mkv.part")
	metaPath := filepath.Join(tmpDir, "prehydrate_deadlock_test.mkv.json")
	cachePath := filepath.Join(tmpDir, "prehydrate_deadlock_test.mkv")

	state := NewFileCacheState(
		"prehydrate_deadlock_test.mkv",
		mockUpstream.URL+"/test.mkv",
		cachePath,
		partPath,
		metaPath,
		"video/mp4",
		"",
		"",
		1024,
	)
	state.IsPrehydrating = true

	doneCh := make(chan struct{})
	go func() {
		StartCoordinator(state, make(http.Header))
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			state.Lock.Lock()
			hasWriter := state.WriterActive
			state.Lock.Unlock()
			state.WorkersLock.Lock()
			workerCount := len(state.ActiveWorkers)
			state.WorkersLock.Unlock()
			if hasWriter && workerCount >= 1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(doneCh)
	}()

	select {
	case <-doneCh:
		// Success: StartCoordinator did not deadlock on state.WorkersLock
	case <-time.After(3 * time.Second):
		t.Fatal("StartCoordinator deadlocked when state.IsPrehydrating == true!")
	}

	CancelCoordinator(state)
}

func TestGlobalRegistryAndFileStateNoLockInversionDeadlock(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "lockinversion_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                  tmpDir,
		TmpDir:                    tmpDir,
		MaxUpstreamWorkers:        2,
		EnableEpisodePrehydration: true,
	}
	defer func() { GlobalConfig = oldConfig }()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 100),
	}

	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("test_file_%d.mkv", i)
		fs := NewFileCacheState(key, "http://example.com/"+key, filepath.Join(tmpDir, key), filepath.Join(tmpDir, key+".part"), filepath.Join(tmpDir, key+".json"), "video/mp4", "", "", 10000000)
		fs.IsPrehydrating = (i%2 == 0)
		fs.IsYielding = (i%2 != 0)
		fs.WriterActive = true
		GlobalRegistry.Files[key] = fs
	}
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Lock.Unlock()
	}()

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Goroutine 1: processNextTask & checkYieldConditions
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				pm.processNextTask()
				pm.checkYieldConditions()
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Goroutine 2: GlobalRegistry write operations (MarkConnectionsAsInCache, UpdateConnectionOffset, GetActiveSessions)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				GlobalRegistry.MarkConnectionsAsInCache("test_file_0.mkv")
				_ = GlobalRegistry.GetActiveSessions()
				GlobalRegistry.UpdateConnectionOffset("test-conn-1", 1024)
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Goroutine 3: Speed monitor lock simulation on FileCacheState
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
				GlobalRegistry.Lock.RLock()
				var states []*FileCacheState
				for _, fs := range GlobalRegistry.Files {
					states = append(states, fs)
				}
				GlobalRegistry.Lock.RUnlock()

				var maxSpeed int64
				for _, fs := range states {
					fs.Lock.Lock()
					fs.BytesDownloadedSec = 1000
					fs.DownloadSpeed = 1000
					if fs.DownloadSpeed > maxSpeed {
						maxSpeed = fs.DownloadSpeed
					}
					fs.Lock.Unlock()
				}

				if maxSpeed > 0 {
					GlobalRegistry.Lock.Lock()
					if maxSpeed > GlobalRegistry.MaxObservedSpeed {
						GlobalRegistry.MaxObservedSpeed = maxSpeed
					}
					GlobalRegistry.Lock.Unlock()
				}
				time.Sleep(1 * time.Millisecond)
			}
		}
	}()

	// Let the goroutines contend for 500ms
	time.Sleep(500 * time.Millisecond)
	close(done)

	// Wait with timeout
	testFinished := make(chan struct{})
	go func() {
		wg.Wait()
		close(testFinished)
	}()

	select {
	case <-testFinished:
		// Success! No deadlock occurred.
	case <-time.After(3 * time.Second):
		t.Fatal("Deadlock detected between GlobalRegistry.Lock and FileCacheState.Lock!")
	}
}

func TestHeadRequestSkipsPrehydrationAndHydration(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "head_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                  tmpDir,
		TmpDir:                    tmpDir,
		MaxUpstreamWorkers:        2,
		EnableEpisodePrehydration: true,
	}
	defer func() { GlobalConfig = oldConfig }()

	oldPM := GlobalPrehydrationManager
	defer func() { GlobalPrehydrationManager = oldPM }()

	mockPM := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}
	GlobalPrehydrationManager = mockPM

	// 1. HEAD on Cache HIT: serves cached headers, must NOT enqueue next_url
	testURL := "http://example.com/show/s02e01.mkv"
	key, ext := getCacheKeyAndExt(testURL)
	cacheFileName := key + ext
	filePath := filepath.Join(tmpDir, cacheFileName)
	testContent := []byte("fake video content for s02e01")
	if err := os.WriteFile(filePath, testContent, 0644); err != nil {
		t.Fatalf("Failed to write mock cache file: %v", err)
	}
	defer os.Remove(filePath)

	reqHit := httptest.NewRequest("HEAD", fmt.Sprintf("/stream?url=%s&next_url=%s",
		testURL, "http://example.com/show/s02e02.mkv"), nil)
	recHit := httptest.NewRecorder()

	handleMediaStream(recHit, reqHit)

	if recHit.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for HEAD cache hit, got %d", recHit.Code)
	}
	if recHit.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("Expected X-Cache: HIT, got %s", recHit.Header().Get("X-Cache"))
	}

	mockPM.mu.Lock()
	if len(mockPM.queue) != 0 {
		mockPM.mu.Unlock()
		t.Fatalf("Expected 0 prehydration tasks queued for HEAD cache hit, got %d", len(mockPM.queue))
	}
	mockPM.mu.Unlock()

	// 2. HEAD on Cache MISS: probes upstream headers, must NOT start coordinator or enqueue next_url
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "50000000")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamServer.Close()

	missURL := upstreamServer.URL + "/ep1.mp4"
	missKey, missExt := getCacheKeyAndExt(missURL)
	missCacheName := missKey + missExt

	reqMiss := httptest.NewRequest("HEAD", fmt.Sprintf("/stream?url=%s&next_url=%s",
		missURL, upstreamServer.URL+"/ep2.mp4"), nil)
	recMiss := httptest.NewRecorder()

	handleMediaStream(recMiss, reqMiss)

	if recMiss.Code != http.StatusOK {
		t.Fatalf("Expected 200 OK for HEAD cache miss, got %d", recMiss.Code)
	}
	if recMiss.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("Expected X-Cache: MISS, got %s", recMiss.Header().Get("X-Cache"))
	}
	if recMiss.Header().Get("Content-Length") != "50000000" {
		t.Fatalf("Expected Content-Length: 50000000, got %s", recMiss.Header().Get("Content-Length"))
	}

	mockPM.mu.Lock()
	if len(mockPM.queue) != 0 {
		mockPM.mu.Unlock()
		t.Fatalf("Expected 0 prehydration tasks queued for HEAD cache miss, got %d", len(mockPM.queue))
	}
	mockPM.mu.Unlock()

	// Verify no active FileCacheState was registered for HEAD request
	GlobalRegistry.Lock.RLock()
	_, exists := GlobalRegistry.Files[missCacheName]
	GlobalRegistry.Lock.RUnlock()
	if exists {
		t.Fatalf("Expected no FileCacheState to be created in GlobalRegistry for HEAD request, but found %s", missCacheName)
	}
}

func TestDeferredPartFileCreationAndProbeBypass(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "probe_bypass_test_*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	oldWorkers := GlobalConfig.MaxUpstreamWorkers
	oldThreshold := GlobalConfig.MinReadThresholdBytes
	oldMaxBytes := GlobalConfig.CacheMaxBytes
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	GlobalConfig.MaxUpstreamWorkers = 1
	GlobalConfig.MinReadThresholdBytes = 256 * 1024
	GlobalConfig.CacheMaxBytes = 50 * 1024 * 1024
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.MaxUpstreamWorkers = oldWorkers
		GlobalConfig.MinReadThresholdBytes = oldThreshold
		GlobalConfig.CacheMaxBytes = oldMaxBytes
		GlobalConfig.Unlock()
	}()

	oldPM := GlobalPrehydrationManager
	defer func() { GlobalPrehydrationManager = oldPM }()

	mockPM := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}
	GlobalPrehydrationManager = mockPM

	// Create 10MB test payload
	testPayload := make([]byte, 10*1024*1024)
	for i := range testPayload {
		testPayload[i] = byte(i % 256)
	}

	blockUpstream := true
	var blockMu sync.Mutex
	unblockCh := make(chan struct{})

	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(testPayload)))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)

		blockMu.Lock()
		shouldBlock := blockUpstream
		blockMu.Unlock()

		if shouldBlock {
			// Write first 64KB (headers/initial chunks) and wait
			_, _ = w.Write(testPayload[:64*1024])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-unblockCh:
				_, _ = w.Write(testPayload[64*1024:])
			case <-r.Context().Done():
				return
			}
		} else {
			_, _ = w.Write(testPayload)
		}
	}))
	defer upstreamServer.Close()

	streamURL := upstreamServer.URL + "/show_ep1.mp4"
	key, ext := getCacheKeyAndExt(streamURL)
	cacheFileName := key + ext
	partPath := filepath.Join(tmpDir, cacheFileName+".part")
	metaPath := filepath.Join(tmpDir, cacheFileName+".json")

	// Phase 1: Client simulates XC series view probe (reads 64KB < 256KB threshold, then disconnects)
	req1 := httptest.NewRequest("GET", fmt.Sprintf("/stream?url=%s&next_url=%s",
		streamURL, upstreamServer.URL+"/show_ep2.mp4"), nil)
	ctx1, cancel1 := context.WithCancel(context.Background())
	req1 = req1.WithContext(ctx1)

	rec1 := httptest.NewRecorder()

	doneCh1 := make(chan struct{})
	go func() {
		defer close(doneCh1)
		handleMediaStream(rec1, req1)
	}()

	// Wait for coordinator to start and buffer initial chunks into LiveBuffer in RAM
	time.Sleep(100 * time.Millisecond)

	// Verify .part file has NOT been created on disk!
	if FileExists(partPath) {
		t.Fatalf("Part file %s was created on disk prematurely during RAM-only probe phase!", partPath)
	}
	if FileExists(metaPath) {
		t.Fatalf("Meta file %s was created on disk prematurely during RAM-only probe phase!", metaPath)
	}

	// Client closes connection (probe finished)
	cancel1()
	<-doneCh1

	// Give reader cleanup a moment
	time.Sleep(50 * time.Millisecond)

	// Verify NO part file or meta file remains on disk
	if FileExists(partPath) {
		t.Fatalf("Part file %s exists on disk after probe disconnect!", partPath)
	}
	if FileExists(metaPath) {
		t.Fatalf("Meta file %s exists on disk after probe disconnect!", metaPath)
	}

	// Verify next episode was NOT enqueued
	mockPM.mu.Lock()
	if len(mockPM.queue) != 0 {
		mockPM.mu.Unlock()
		t.Fatalf("Expected 0 prehydration tasks queued for aborted probe, got %d", len(mockPM.queue))
	}
	mockPM.mu.Unlock()

	// Verify state removed from GlobalRegistry
	GlobalRegistry.Lock.RLock()
	_, exists := GlobalRegistry.Files[cacheFileName]
	GlobalRegistry.Lock.RUnlock()
	if exists {
		t.Fatalf("Expected FileCacheState to be deleted from registry on probe exit")
	}

	// Unblock upstream server for Phase 2 (real playback)
	blockMu.Lock()
	blockUpstream = false
	blockMu.Unlock()
	close(unblockCh)

	// Phase 2: Client plays video and streams past threshold (reads 512KB >= 256KB threshold)
	req2 := httptest.NewRequest("GET", fmt.Sprintf("/stream?url=%s&next_url=%s",
		streamURL, upstreamServer.URL+"/show_ep2.mp4"), nil)
	rec2 := httptest.NewRecorder()

	ctx2, cancel2 := context.WithCancel(context.Background())
	req2 = req2.WithContext(ctx2)

	doneCh2 := make(chan struct{})
	go func() {
		defer close(doneCh2)
		handleMediaStream(rec2, req2)
	}()

	// Wait for streaming and reader to cross threshold
	time.Sleep(150 * time.Millisecond)

	// Fetch registered state and simulate client reading 512KB
	GlobalRegistry.Lock.RLock()
	state2 := GlobalRegistry.Files[cacheFileName]
	GlobalRegistry.Lock.RUnlock()

	if state2 != nil {
		reader := NewCacheReader(ctx2, state2, req2)
		buf := make([]byte, 512*1024)
		_, _ = reader.ReadAt(buf, 0)
		_ = reader.Close()
	}

	cancel2()
	<-doneCh2
	time.Sleep(100 * time.Millisecond)

	// Now that read threshold was crossed, part file or cached file MUST be materialized on disk!
	if !FileExists(partPath) && !FileExists(filepath.Join(tmpDir, cacheFileName)) {
		t.Fatalf("Expected part file %s to be materialized on disk once read threshold was crossed", partPath)
	}

	if state2 != nil {
		CancelCoordinator(state2)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		GlobalRegistry.Lock.RLock()
		curState, inReg := GlobalRegistry.Files[cacheFileName]
		GlobalRegistry.Lock.RUnlock()
		status := ""
		writerActive := false
		if curState != nil {
			curState.Lock.Lock()
			status = curState.UpstreamStatus
			writerActive = curState.WriterActive
			curState.Lock.Unlock()
		}
		if (!inReg || status != "Committing") && !writerActive && (GlobalLogBroker == nil || GlobalLogBroker.broadcasting.Load() == 0) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestProbeSuppressionAndIndicatorInDashboard(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "sc_dashboard_probe_test_*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.Unlock()
	}()

	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	oldSessions := GlobalRegistry.Sessions
	oldConns := GlobalRegistry.Connections
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	GlobalRegistry.Sessions = make(map[string]*ClientSession)
	GlobalRegistry.Connections = make(map[string]*ClientConnection)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		GlobalRegistry.Sessions = oldSessions
		GlobalRegistry.Connections = oldConns
		GlobalRegistry.Lock.Unlock()
	}()

	// 1. Create an unmaterialized probe file state (DiskMaterialized = false)
	probeState := &FileCacheState{
		Key:              "probe_key_1",
		TotalSize:        10 * 1024 * 1024,
		RangeSet:         NewRangeSet(),
		Clients:          make(map[string]chan struct{}),
		Starving:         make(map[string]int64),
		StarvingSince:    make(map[string]time.Time),
		DiskMaterialized: false,
		IsPrehydrating:   false,
		UpstreamStatus:   "Downloading",
	}

	// 2. Create a materialized normal playback file state (DiskMaterialized = true)
	playbackState := &FileCacheState{
		Key:              "playback_key_2",
		TotalSize:        20 * 1024 * 1024,
		RangeSet:         NewRangeSet(),
		Clients:          make(map[string]chan struct{}),
		Starving:         make(map[string]int64),
		StarvingSince:    make(map[string]time.Time),
		DiskMaterialized: true,
		IsPrehydrating:   false,
		UpstreamStatus:   "Downloading",
	}

	// 3. Register client connections
	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[probeState.Key] = probeState
	GlobalRegistry.Files[playbackState.Key] = playbackState
	GlobalRegistry.Lock.Unlock()

	conn1 := &ClientConnection{
		ID:             "client-vod-1",
		Key:            playbackState.Key,
		URL:            "http://example.com/stream.mp4",
		RemoteAddr:     "192.168.1.50:1111",
		UserAgent:      "VLC/3.0.20",
		Offset:         5000,
		TotalSize:      playbackState.TotalSize,
		StartTime:      time.Now().Unix(),
		InCache:        false,
		StarvingOffset: -1,
	}
	GlobalRegistry.RegisterConnection("client-vod-1", conn1)

	// Verify Probe is active
	if !GlobalRegistry.IsProbeActive() {
		t.Fatalf("Expected IsProbeActive to be true with unmaterialized probe in registry")
	}

	// Generate dashboard JSON
	rawJSON, err := buildStatePayload()
	if err != nil {
		t.Fatalf("Failed to build state payload: %v", err)
	}

	var stateMap map[string]interface{}
	if err := json.Unmarshal(rawJSON, &stateMap); err != nil {
		t.Fatalf("Failed to unmarshal dashboard JSON: %v", err)
	}

	// Check probe_active in JSON
	probeActive, ok := stateMap["probe_active"].(bool)
	if !ok || !probeActive {
		t.Fatalf("Expected probe_active to be true in dashboard JSON, got %v", stateMap["probe_active"])
	}

	// Check active_vod_clients (1 active, 1 closed)
	vodClients, ok := stateMap["active_vod_clients"].(float64)
	if !ok || int(vodClients) != 1 {
		t.Fatalf("Expected active_vod_clients to be 1, got %v", stateMap["active_vod_clients"])
	}

	// Check active_vod_probes (1 unmaterialized probe state)
	vodProbes, ok := stateMap["active_vod_probes"].(float64)
	if !ok || int(vodProbes) != 1 {
		t.Fatalf("Expected active_vod_probes to be 1, got %v", stateMap["active_vod_probes"])
	}

	// Verify active_downloads ONLY includes playbackState, and NOT probeState!
	activeDownloads, ok := stateMap["active_downloads"].([]interface{})
	if !ok {
		t.Fatalf("Expected active_downloads to be a slice, got %T", stateMap["active_downloads"])
	}
	if len(activeDownloads) != 1 {
		t.Fatalf("Expected exactly 1 active download (probe suppressed), got %d: %v", len(activeDownloads), activeDownloads)
	}
	item := activeDownloads[0].(map[string]interface{})
	if item["key"] != "playback_key_2" {
		t.Fatalf("Expected active download to be playback_key_2, got %v", item["key"])
	}
}

func TestDynamicCapacityAccounting(t *testing.T) {
	oldRes := GlobalConfig.GetPrehydrateCapacityReservation()
	oldPrefetch := GlobalConfig.GetEnableEpisodePrehydration()
	GlobalConfig.SetPrehydrateCapacityReservation(1)
	GlobalConfig.SetEnableEpisodePrehydration(true)
	defer func() {
		GlobalConfig.SetPrehydrateCapacityReservation(oldRes)
		GlobalConfig.SetEnableEpisodePrehydration(oldPrefetch)
	}()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		latestCapacity: &ProviderCapacityReport{
			Status:                 "ok",
			TotalMaxStreams:        5,
			TotalActiveStreams:     2,
			TotalAvailableStreams:  3,
			TargetAvailableStreams: 3,
		},
	}

	// Available = 3, Reservation = 1.
	// Can start prehydration 1: (3 - 0) >= 2 -> true
	if !pm.CanStartPrehydration("http://example.com/ep1") {
		t.Fatalf("Expected CanStartPrehydration=true initially")
	}

	// Simulate starting prehydration 1
	pm.mu.Lock()
	pm.activePrehydrations["ep1"] = true
	pm.prehydrationsStartedSinceLastPoll++
	pm.mu.Unlock()

	// Prehydration 2: effective available = 3 - 1 = 2 >= 2 -> true
	if !pm.CanStartPrehydration("http://example.com/ep2") {
		t.Fatalf("Expected CanStartPrehydration=true for 2nd prehydration")
	}

	// Simulate starting prehydration 2
	pm.mu.Lock()
	pm.activePrehydrations["ep2"] = true
	pm.prehydrationsStartedSinceLastPoll++
	pm.mu.Unlock()

	// Prehydration 3: effective available = 3 - 2 = 1 < 2 -> false (reservation preserved for live viewers!)
	if pm.CanStartPrehydration("http://example.com/ep3") {
		t.Fatalf("Expected CanStartPrehydration=false when capacity reservation threshold is reached")
	}

	// Capacity poll arrives and resets started counter (with updated provider metrics)
	pm.mu.Lock()
	pm.prehydrationsStartedSinceLastPoll = 0
	pm.latestCapacity = &ProviderCapacityReport{
		Status:                 "ok",
		TotalMaxStreams:        5,
		TotalActiveStreams:     4,
		TotalAvailableStreams:  1,
		TargetAvailableStreams: 1,
	}
	pm.mu.Unlock()

	// Now available is 1, reservation 1 -> 1 >= 2 is false
	if pm.CanStartPrehydration("http://example.com/ep3") {
		t.Fatalf("Expected CanStartPrehydration=false after capacity poll confirmed streams are active")
	}
}

func TestYieldDeficiencySelectivePacing(t *testing.T) {
	oldRes := GlobalConfig.GetPrehydrateCapacityReservation()
	GlobalConfig.SetPrehydrateCapacityReservation(1)
	defer GlobalConfig.SetPrehydrateCapacityReservation(oldRes)

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		latestCapacity: &ProviderCapacityReport{
			Status:                 "ok",
			TotalMaxStreams:        5,
			TotalActiveStreams:     5,
			TotalAvailableStreams:  0, // 0 available < reservation 1 -> deficiency of 1!
			TargetAvailableStreams: 0,
		},
	}

	tmpDir := t.TempDir()
	fs1 := NewFileCacheState("f1.mkv", "http://example.com/f1", filepath.Join(tmpDir, "f1.mkv"), filepath.Join(tmpDir, "f1.mkv.part"), filepath.Join(tmpDir, "f1.mkv.json"), "video/mp4", "", "", 100)
	fs1.IsPrehydrating = true
	fs1.WriterActive = true
	fs1.SetUpstreamStatus("Prehydrating")

	fs2 := NewFileCacheState("f2.mkv", "http://example.com/f2", filepath.Join(tmpDir, "f2.mkv"), filepath.Join(tmpDir, "f2.mkv.part"), filepath.Join(tmpDir, "f2.mkv.json"), "video/mp4", "", "", 100)
	fs2.IsPrehydrating = true
	fs2.WriterActive = true
	fs2.SetUpstreamStatus("Prehydrating")

	fs3 := NewFileCacheState("f3.mkv", "http://example.com/f3", filepath.Join(tmpDir, "f3.mkv"), filepath.Join(tmpDir, "f3.mkv.part"), filepath.Join(tmpDir, "f3.mkv.json"), "video/mp4", "", "", 100)
	fs3.IsPrehydrating = true
	fs3.WriterActive = true
	fs3.SetUpstreamStatus("Prehydrating")

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[fs1.Key] = fs1
	GlobalRegistry.Files[fs2.Key] = fs2
	GlobalRegistry.Files[fs3.Key] = fs3
	GlobalRegistry.Lock.Unlock()

	defer func() {
		CancelCoordinator(fs1)
		CancelCoordinator(fs2)
		CancelCoordinator(fs3)
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, fs1.Key)
		delete(GlobalRegistry.Files, fs2.Key)
		delete(GlobalRegistry.Files, fs3.Key)
		GlobalRegistry.Lock.Unlock()
	}()

	// Run checkYieldConditions. Since available=0 and reservation=1, deficiency=1.
	// Exactly 1 prehydration should yield, NOT all 3!
	pm.checkYieldConditions()

	yieldedCount := 0
	if fs1.IsYielding {
		yieldedCount++
	}
	if fs2.IsYielding {
		yieldedCount++
	}
	if fs3.IsYielding {
		yieldedCount++
	}

	if yieldedCount != 1 {
		t.Fatalf("Expected exactly 1 file to yield to satisfy reservation deficiency, got %d", yieldedCount)
	}
}

func TestConcurrentYieldedReactivation(t *testing.T) {
	oldRes := GlobalConfig.GetPrehydrateCapacityReservation()
	GlobalConfig.SetPrehydrateCapacityReservation(1)
	defer GlobalConfig.SetPrehydrateCapacityReservation(oldRes)

	oldPM := GlobalPrehydrationManager

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 100))
	}))
	defer ts.Close()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		latestCapacity: &ProviderCapacityReport{
			Status:                 "ok",
			TotalMaxStreams:        6,
			TotalActiveStreams:     2,
			TotalAvailableStreams:  4, // 4 available, 1 reserved -> can run 3 prehydrations!
			TargetAvailableStreams: 4,
		},
	}
	GlobalPrehydrationManager = pm

	tmpDir := t.TempDir()
	fs1 := NewFileCacheState("f1.mkv", ts.URL+"/f1", filepath.Join(tmpDir, "f1.mkv"), filepath.Join(tmpDir, "f1.mkv.part"), filepath.Join(tmpDir, "f1.mkv.json"), "video/mp4", "", "", 100)
	fs1.IsYielding = true
	fs1.YieldCooldownUntil = time.Now().Add(-1 * time.Second)

	fs2 := NewFileCacheState("f2.mkv", ts.URL+"/f2", filepath.Join(tmpDir, "f2.mkv"), filepath.Join(tmpDir, "f2.mkv.part"), filepath.Join(tmpDir, "f2.mkv.json"), "video/mp4", "", "", 100)
	fs2.IsYielding = true
	fs2.YieldCooldownUntil = time.Now().Add(-1 * time.Second)

	fs3 := NewFileCacheState("f3.mkv", ts.URL+"/f3", filepath.Join(tmpDir, "f3.mkv"), filepath.Join(tmpDir, "f3.mkv.part"), filepath.Join(tmpDir, "f3.mkv.json"), "video/mp4", "", "", 100)
	fs3.IsYielding = true
	fs3.YieldCooldownUntil = time.Now().Add(-1 * time.Second)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[fs1.Key] = fs1
	GlobalRegistry.Files[fs2.Key] = fs2
	GlobalRegistry.Files[fs3.Key] = fs3
	GlobalRegistry.Lock.Unlock()

	defer func() {
		CancelCoordinator(fs1)
		CancelCoordinator(fs2)
		CancelCoordinator(fs3)
		for _, fs := range []*FileCacheState{fs1, fs2, fs3} {
			fs.coordMu.Lock()
			fs.coordMu.Unlock()
		}
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, fs1.Key)
		delete(GlobalRegistry.Files, fs2.Key)
		delete(GlobalRegistry.Files, fs3.Key)
		GlobalRegistry.Lock.Unlock()
		GlobalPrehydrationManager = oldPM
	}()

	// Execute processNextTask.
	// Since 3 slots are available above reservation (4 - 1 = 3),
	// ALL 3 yielded prehydrations should reactivate concurrently!
	pm.processNextTask()

	if fs1.IsYielding || !fs1.IsPrehydrating {
		t.Fatalf("Expected fs1 to be reactivated (IsYielding=false, IsPrehydrating=true)")
	}
	if fs2.IsYielding || !fs2.IsPrehydrating {
		t.Fatalf("Expected fs2 to be reactivated (IsYielding=false, IsPrehydrating=true)")
	}
	if fs3.IsYielding || !fs3.IsPrehydrating {
		t.Fatalf("Expected fs3 to be reactivated (IsYielding=false, IsPrehydrating=true)")
	}

	pm.mu.RLock()
	activeCount := len(pm.activePrehydrations)
	startedCount := pm.prehydrationsStartedSinceLastPoll
	pm.mu.RUnlock()

	if activeCount != 3 {
		t.Fatalf("Expected 3 active prehydrations, got %d", activeCount)
	}
	if startedCount != 3 {
		t.Fatalf("Expected 3 prehydrations started since last poll, got %d", startedCount)
	}
}

func TestYieldedCoordinatorPreservesPartFileAndRegistryEntry(t *testing.T) {
	tmpDir := t.TempDir()

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                  tmpDir,
		TmpDir:                    tmpDir,
		MaxUpstreamWorkers:        2,
		EnableEpisodePrehydration: true,
		WritebackBufferSize:       64 * 1024,
	}
	defer func() { GlobalConfig = oldConfig }()

	blockCh := make(chan struct{})

	firstChunkSent := make(chan struct{}, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte("A"), 4096))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case firstChunkSent <- struct{}{}:
		default:
		}
		select {
		case <-blockCh:
		case <-r.Context().Done():
		}
	}))
	defer ts.Close()
	defer close(blockCh)

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}
	oldPM := GlobalPrehydrationManager
	GlobalPrehydrationManager = pm
	defer func() { GlobalPrehydrationManager = oldPM }()

	key := "yield_preserve.mkv"
	fs := NewFileCacheState(
		key,
		ts.URL+"/yield_preserve.mkv",
		filepath.Join(tmpDir, key),
		filepath.Join(tmpDir, key+".part"),
		filepath.Join(tmpDir, key+".json"),
		"video/mp4",
		"",
		"",
		1048576,
	)
	fs.IsPrehydrating = true
	pm.activePrehydrations[key] = true

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[key] = fs
	GlobalRegistry.Lock.Unlock()

	defer func() {
		CancelCoordinator(fs)
		fs.coordMu.Lock()
		fs.coordMu.Unlock()
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, key)
		GlobalRegistry.Lock.Unlock()
	}()

	StartCoordinator(fs, make(http.Header))

	select {
	case <-firstChunkSent:
	case <-time.After(2 * time.Second):
		t.Fatal("Timed out waiting for coordinator worker to receive initial chunk")
	}

	// Give worker a moment to enqueue into WritebackPipeline
	time.Sleep(30 * time.Millisecond)

	// Yield the prehydrating state
	pm.YieldState(fs, "test")

	// Wait for runCoordinator to exit completely by locking coordMu
	fs.coordMu.Lock()
	fs.coordMu.Unlock()

	// Verify the yielded file was NOT deleted from GlobalRegistry or disk!
	GlobalRegistry.Lock.RLock()
	regState, exists := GlobalRegistry.Files[key]
	GlobalRegistry.Lock.RUnlock()
	if !exists || regState != fs {
		t.Fatalf("Expected yielded file %s to remain in GlobalRegistry.Files, got exists=%v", key, exists)
	}
	if !FileExists(fs.PartPath) {
		t.Fatalf("Expected yielded .part file %s to remain on disk after coordinator exit", fs.PartPath)
	}
	fs.Lock.Lock()
	isYielding := fs.IsYielding
	downloaded := fs.RangeSet.TotalBytes()
	fs.Lock.Unlock()
	if !isYielding {
		t.Fatalf("Expected fs.IsYielding to remain true")
	}
	if downloaded < 4096 {
		t.Fatalf("Expected WritebackPipeline to flush >= 4096 bytes before coordinator exit, got %d", downloaded)
	}
}

func TestPriorityUpgradeWhileSSEActiveNoSelfDeadlock(t *testing.T) {
	tmpDir := t.TempDir()

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                  tmpDir,
		TmpDir:                    tmpDir,
		MaxUpstreamWorkers:        2,
		EnableEpisodePrehydration: true,
		WritebackBufferSize:       64 * 1024,
	}

	if GlobalLogBroker == nil {
		InitLogBroker()
	}
	sseCh := make(chan SSEEvent, 100)
	GlobalLogBroker.Register(sseCh)

	payload := bytes.Repeat([]byte("V"), 8192)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}
	oldPM := GlobalPrehydrationManager
	GlobalPrehydrationManager = pm

	targetURL := ts.URL + "/priority_upgrade.mkv"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext

	fs := NewFileCacheState(
		cacheFileName,
		targetURL,
		filepath.Join(tmpDir, cacheFileName),
		filepath.Join(tmpDir, cacheFileName+".part"),
		filepath.Join(tmpDir, cacheFileName+".json"),
		"video/mp4",
		"",
		"",
		int64(len(payload)),
	)
	// Simulate a yielded prehydration file (WriterActive == false, IsYielding == true)
	fs.IsPrehydrating = false
	fs.IsYielding = true
	fs.WriterActive = false
	pm.activePrehydrations[cacheFileName] = true
	pm.prehydrationsStartedSinceLastPoll = 1

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files[cacheFileName] = fs
	GlobalRegistry.Lock.Unlock()

	defer func() {
		// Wait briefly if committing so finalizeAndCommitCache finishes before restoring globals
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			fs.Lock.Lock()
			status := fs.UpstreamStatus
			fs.Lock.Unlock()
			if status != "Committing" {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		CancelCoordinator(fs)
		fs.coordMu.Lock()
		fs.coordMu.Unlock()
		GlobalLogBroker.Unregister(sseCh)
		for GlobalLogBroker.broadcasting.Load() != 0 {
			time.Sleep(5 * time.Millisecond)
		}
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, cacheFileName)
		GlobalRegistry.Lock.Unlock()
		GlobalPrehydrationManager = oldPM
		GlobalConfig = oldConfig
	}()

	req := httptest.NewRequest(http.MethodGet, "/stream?url="+url.QueryEscape(targetURL), nil)
	rec := httptest.NewRecorder()

	doneCh := make(chan struct{})
	go func() {
		handleMediaStream(rec, req)
		close(doneCh)
	}()

	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("handleMediaStream deadlocked during Priority Upgrade of a yielded file!")
	}

	if rec.Code != http.StatusOK && rec.Code != http.StatusPartialContent {
		t.Fatalf("Expected status 200 or 206, got %d", rec.Code)
	}
	if rec.Body.Len() != len(payload) {
		t.Fatalf("Expected %d bytes in response body, got %d", len(payload), rec.Body.Len())
	}

	pm.mu.RLock()
	stillActive := pm.activePrehydrations[cacheFileName]
	pm.mu.RUnlock()
	if stillActive {
		t.Fatalf("Expected Priority Upgrade to remove %s from activePrehydrations", cacheFileName)
	}
}

func TestFailedPrehydrationProbeDoesNotRegisterHydrationCard(t *testing.T) {
	tmpDir := t.TempDir()

	oldConfig := GlobalConfig
	GlobalConfig = &Config{
		CacheDir:                  tmpDir,
		TmpDir:                    tmpDir,
		MaxUpstreamWorkers:        2,
		EnableEpisodePrehydration: true,
		PrehydrateYieldBackoff:    10 * time.Second,
	}
	defer func() { GlobalConfig = oldConfig }()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
	}
	oldPM := GlobalPrehydrationManager
	GlobalPrehydrationManager = pm
	defer func() { GlobalPrehydrationManager = oldPM }()

	targetURL := ts.URL + "/failed_prehydrate_probe.mkv"
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext

	pm.Enqueue(targetURL, []string{ts.URL + "/next_ep.mkv"})
	pm.processNextTask()

	// 1. Verify no FileCacheState was registered in GlobalRegistry.Files
	GlobalRegistry.Lock.RLock()
	_, exists := GlobalRegistry.Files[cacheFileName]
	GlobalRegistry.Lock.RUnlock()
	if exists {
		t.Fatalf("Expected failed prehydration probe NOT to register a FileCacheState card in GlobalRegistry.Files")
	}

	// 2. Verify no FailedProbe tombstone was recorded (so live viewers are not blocked)
	if GlobalRegistry.IsProbeFailed(cacheFileName) {
		t.Fatalf("Expected failed prehydration probe NOT to record a FailedProbe tombstone")
	}

	// 3. Verify tentative slot was released and task was re-queued with NotBefore backoff
	pm.mu.RLock()
	started := pm.prehydrationsStartedSinceLastPoll
	qLen := len(pm.queue)
	var requeued *PrehydrationTask
	if qLen > 0 {
		requeued = pm.queue[0]
	}
	pm.mu.RUnlock()

	if started != 0 {
		t.Fatalf("Expected prehydrationsStartedSinceLastPoll to be 0 after failed probe, got %d", started)
	}
	if qLen != 1 || requeued == nil || requeued.ProbeAttempts != 1 || requeued.NotBefore.IsZero() {
		t.Fatalf("Expected transient probe failure to re-queue task with ProbeAttempts=1 and NotBefore set")
	}
}

func TestLiveBufferPinsInFlightChunksWithRefCountGreaterThanOne(t *testing.T) {
	// Create a LiveBuffer with 4096-byte max capacity
	lb := NewLiveBuffer(4096)

	// Insert chunk 1 at offset 0 with refCount = 2 (in-flight to disk)
	data1 := bytes.Repeat([]byte("1"), 4096)
	chunk1 := newPooledChunk(0, data1, 2)
	lb.Put(chunk1)

	// Insert chunk 2 at offset 4096 with refCount = 2 (also in-flight to disk)
	// Because chunk 1 has refCount == 2, LiveBuffer must keep chunk 1 pinned in RAM!
	data2 := bytes.Repeat([]byte("2"), 4096)
	chunk2 := newPooledChunk(4096, data2, 2)
	lb.Put(chunk2)

	buf := make([]byte, 4096)
	if n, ok := lb.ReadAt(buf, 0); !ok || n != 4096 || buf[0] != '1' {
		t.Fatalf("Expected in-flight chunk 1 (refCount=2) to remain pinned in LiveBuffer, got ok=%v n=%d", ok, n)
	}

	// Now simulate storageConsumer finishing disk write for chunk 1 (drops refCount from 2 -> 1)
	chunk1.release()

	// Insert chunk 3 at offset 8192 with refCount = 2; now chunk 1 (refCount == 1) can be evicted!
	data3 := bytes.Repeat([]byte("3"), 4096)
	chunk3 := newPooledChunk(8192, data3, 2)
	lb.Put(chunk3)

	if _, ok := lb.ReadAt(buf, 0); ok {
		t.Fatalf("Expected chunk 1 (refCount=1) to be evicted once capacity was exceeded")
	}
	if n, ok := lb.ReadAt(buf, 4096); !ok || n != 4096 || buf[0] != '2' {
		t.Fatalf("Expected in-flight chunk 2 (refCount=2) to remain pinned in LiveBuffer")
	}

	chunk2.release()
	chunk3.release()
	lb.Clear()
}

func TestLiveBufferSmallReadsCoalesceInto128KBChunks(t *testing.T) {
	lb := NewLiveBuffer(64 * 1024 * 1024)
	defer lb.Clear()

	smallSlice := bytes.Repeat([]byte("A"), 3000)
	var offset int64
	for i := 0; i < 2000; i++ {
		req := lb.PutSlice(offset, smallSlice, false)
		if req.length != 3000 {
			t.Fatalf("Expected writeReq length 3000, got %d", req.length)
		}
		offset += 3000
	}

	lb.mu.RLock()
	numChunks := len(lb.chunks)
	curBytes := lb.curBytes
	lb.mu.RUnlock()

	if curBytes != 6000000 {
		t.Fatalf("Expected curBytes=6000000, got %d", curBytes)
	}
	// 6,000,000 bytes / 131,072 bytes per chunk = 45.77 -> 46 chunks (instead of 2,000 chunks!)
	if numChunks > 50 {
		t.Fatalf("Expected small slices to coalesce into <= 50 128KB chunks, got %d chunks", numChunks)
	}

	readBuf := make([]byte, 10000)
	if n, ok := lb.ReadAt(readBuf, 129000); !ok || n != 10000 {
		t.Fatalf("Expected cross-chunk ReadAt to succeed with 10000 bytes, got ok=%v n=%d", ok, n)
	}
}

func TestPreEvictAccountsForConcurrentActiveUnwrittenBytes(t *testing.T) {
	tmpDir := t.TempDir()

	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldMaxBytes := GlobalConfig.CacheMaxBytes
	oldMinFree := GlobalConfig.MinDiskFreeBytes
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.CacheMaxBytes = 300
	GlobalConfig.MinDiskFreeBytes = 0
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.CacheMaxBytes = oldMaxBytes
		GlobalConfig.MinDiskFreeBytes = oldMinFree
		GlobalConfig.Unlock()
	}()

	GlobalRegistry.Lock.Lock()
	oldFiles := GlobalRegistry.Files
	oldTotal := atomic.LoadInt64(&GlobalRegistry.TotalCacheSize)
	GlobalRegistry.Files = make(map[string]*FileCacheState)
	atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, 230)
	GlobalRegistry.Lock.Unlock()

	defer func() {
		GlobalRegistry.Lock.Lock()
		GlobalRegistry.Files = oldFiles
		atomic.StoreInt64(&GlobalRegistry.TotalCacheSize, oldTotal)
		GlobalRegistry.Lock.Unlock()
	}()

	// Write two inactive 100-byte files in cacheDir
	for i, name := range []string{"old1.mkv", "old2.mkv"} {
		p := filepath.Join(tmpDir, name)
		if err := os.WriteFile(p, bytes.Repeat([]byte("x"), 100), 0644); err != nil {
			t.Fatal(err)
		}
		meta := FileMeta{
			URL:           "http://example.com/" + name,
			TotalSize:     100,
			Completed:     true,
			LastValidated: time.Now().Add(time.Duration(i-2) * time.Hour).Unix(),
		}
		mb, _ := json.Marshal(meta)
		_ = os.WriteFile(p+".json", mb, 0644)
	}

	// Register an active materialized file with TotalSize=150 and TotalBytesDownloaded=30 (120 bytes unwritten)
	activeState := NewFileCacheState(
		"active1.mkv",
		"http://example.com/active1.mkv",
		filepath.Join(tmpDir, "active1.mkv"),
		filepath.Join(tmpDir, "active1.mkv.part"),
		filepath.Join(tmpDir, "active1.mkv.json"),
		"video/x-matroska",
		"",
		"",
		150,
	)
	activeState.DiskMaterialized = true
	atomic.StoreInt64(&activeState.TotalBytesDownloaded, 30)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files["active1.mkv"] = activeState
	GlobalRegistry.Lock.Unlock()

	// Call PreEvict(100) for a second incoming file.
	// Total projected = currentSize(230) + unwrittenActive(120) + incoming(100) = 450 > maxBytes(300) -> must free 150 bytes (both old1 and old2).
	GlobalRegistry.PreEvict(100)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !FileExists(filepath.Join(tmpDir, "old1.mkv")) && !FileExists(filepath.Join(tmpDir, "old2.mkv")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if FileExists(filepath.Join(tmpDir, "old1.mkv")) || FileExists(filepath.Join(tmpDir, "old2.mkv")) {
		t.Fatalf("Expected both old1.mkv and old2.mkv to be evicted when accounting for active unwritten bytes")
	}
}

func TestProbeDisconnectDoesNotAbortMaterializedPrehydration(t *testing.T) {
	tmpDir := t.TempDir()

	GlobalConfig.Lock()
	oldCacheDir := GlobalConfig.CacheDir
	oldTmpDir := GlobalConfig.TmpDir
	oldEnablePre := GlobalConfig.EnableEpisodePrehydration
	GlobalConfig.CacheDir = tmpDir
	GlobalConfig.TmpDir = tmpDir
	GlobalConfig.EnableEpisodePrehydration = true
	GlobalConfig.Unlock()

	defer func() {
		GlobalConfig.Lock()
		GlobalConfig.CacheDir = oldCacheDir
		GlobalConfig.TmpDir = oldTmpDir
		GlobalConfig.EnableEpisodePrehydration = oldEnablePre
		GlobalConfig.Unlock()
	}()

	pm := &PrehydrationManager{
		activePrehydrations: make(map[string]bool),
		stopCh:              make(chan struct{}),
		notifyCh:            make(chan struct{}, 10),
		latestCapacity: &ProviderCapacityReport{
			Status:                 "ok",
			TotalMaxStreams:        10,
			TotalActiveStreams:     2,
			TotalAvailableStreams:  8,
			TargetAvailableStreams: 8,
		},
	}
	oldPM := GlobalPrehydrationManager
	GlobalPrehydrationManager = pm
	defer func() { GlobalPrehydrationManager = oldPM }()

	partPath := filepath.Join(tmpDir, "bigmovie.mkv.part")
	metaPath := filepath.Join(tmpDir, "bigmovie.mkv.json")
	cachePath := filepath.Join(tmpDir, "bigmovie.mkv")

	state := NewFileCacheState(
		"bigmovie.mkv",
		"http://example.com/bigmovie.mkv",
		cachePath,
		partPath,
		metaPath,
		"video/x-matroska",
		"",
		"",
		80*1024*1024,
	)
	state.IsPrehydrating = true
	state.WriterActive = true
	if err := state.MaterializeDiskFile(); err != nil {
		t.Fatal(err)
	}
	// Write 4096 cached bytes at offset 0
	chunk := newPooledChunk(0, bytes.Repeat([]byte("M"), 4096), 1)
	state.LiveBuffer.Put(chunk)
	state.RangeSet.Add(0, 4096)

	GlobalRegistry.Lock.Lock()
	GlobalRegistry.Files["bigmovie.mkv"] = state
	GlobalRegistry.Lock.Unlock()
	defer func() {
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, "bigmovie.mkv")
		GlobalRegistry.Lock.Unlock()
	}()

	pm.mu.Lock()
	pm.activePrehydrations["bigmovie.mkv"] = true
	pm.prehydrationsStartedSinceLastPoll = 1
	pm.mu.Unlock()

	// Phase 1: A probe (<5MB read) connects to the active prehydration.
	// It must NOT alter IsPrehydrating while open or abort the hydration on disconnect.
	req := httptest.NewRequest("GET", "/stream?url=http://example.com/bigmovie.mkv", nil)
	reader := NewCacheReader(context.Background(), state, req)
	buf := make([]byte, 4096)
	if n, err := reader.ReadAt(buf, 0); err != nil || n != 4096 {
		t.Fatalf("Expected probe ReadAt to succeed, got n=%d err=%v", n, err)
	}
	state.Lock.Lock()
	stillPreDuringProbe := state.IsPrehydrating
	state.Lock.Unlock()
	if !stillPreDuringProbe {
		t.Fatalf("Expected <5MB probe NOT to alter state.IsPrehydrating while reading")
	}
	_ = reader.Close()

	// Verify part file was NOT deleted and state remained IsPrehydrating=true!
	if !FileExists(partPath) {
		t.Fatalf("Expected materialized .part file to be preserved after <5MB probe disconnect!")
	}
	state.Lock.Lock()
	isPre := state.IsPrehydrating
	writerActive := state.WriterActive
	state.Lock.Unlock()
	if !isPre || !writerActive {
		t.Fatalf("Expected state to remain active and IsPrehydrating=true, got isPre=%v writerActive=%v", isPre, writerActive)
	}

	// Phase 2: Even if a client had upgraded to live playback and disconnected early, .part is preserved and downshifted.
	state.Lock.Lock()
	state.IsPrehydrating = false
	state.Lock.Unlock()
	pm.OnPriorityUpgrade("bigmovie.mkv")

	reader2 := NewCacheReader(context.Background(), state, req)
	if n, err := reader2.ReadAt(buf, 0); err != nil || n != 4096 {
		t.Fatalf("Expected second ReadAt to succeed, got n=%d err=%v", n, err)
	}
	_ = reader2.Close()

	if !FileExists(partPath) {
		t.Fatalf("Expected materialized .part file to be preserved after upgraded client disconnect!")
	}
	state.Lock.Lock()
	isPreAfterDownshift := state.IsPrehydrating
	state.Lock.Unlock()
	if !isPreAfterDownshift {
		t.Fatalf("Expected state to downshift back to IsPrehydrating=true after upgraded client disconnect")
	}
}






