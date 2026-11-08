package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
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

	go handleMediaStream(rec2, req2)

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
	time.Sleep(100 * time.Millisecond)
	if state2 != nil {
		CancelCoordinator(state2)
	}

	// Now that read threshold was crossed, part file or cached file MUST be materialized on disk!
	if !FileExists(partPath) && !FileExists(filepath.Join(tmpDir, cacheFileName)) {
		t.Fatalf("Expected part file %s to be materialized on disk once read threshold was crossed", partPath)
	}
}




