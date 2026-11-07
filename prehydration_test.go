package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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


