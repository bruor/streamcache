package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type ProviderProfileDetail struct {
	ProfileID        int    `json:"profile_id"`
	Name             string `json:"name"`
	MaxStreams       int    `json:"max_streams"`
	ActiveStreams    int    `json:"active_streams"`
	AvailableStreams int    `json:"available_streams"`
}

type ProviderDetail struct {
	AccountID        int                     `json:"account_id"`
	AccountName      string                  `json:"account_name"`
	MaxStreams       int                     `json:"max_streams"`
	ActiveStreams    int                     `json:"active_streams"`
	AvailableStreams int                     `json:"available_streams"`
	Profiles         []ProviderProfileDetail `json:"profiles"`
}

type ProviderCapacityReport struct {
	Status                 string           `json:"status"`
	Timestamp              int64            `json:"timestamp"`
	TotalMaxStreams        int              `json:"total_max_streams"`
	TotalActiveStreams     int              `json:"total_active_streams"`
	TotalAvailableStreams  int              `json:"total_available_streams"`
	TargetAvailableStreams int              `json:"target_available_streams"`
	Providers              []ProviderDetail `json:"providers"`
	Error                  string           `json:"error,omitempty"`
}

type PrehydrationTask struct {
	TargetURL  string
	NextURLs   []string
	EnqueuedAt time.Time
}

type PrehydrationManager struct {
	mu                       sync.RWMutex
	latestCapacity           *ProviderCapacityReport
	lastPollTime             time.Time
	discoveredDispatcharrURL string
	queue                    []*PrehydrationTask
	activePrehydrations      map[string]bool // Set of canonical cacheFileNames actively running prehydration
	stopCh                   chan struct{}
	notifyCh                 chan struct{}
}

var GlobalPrehydrationManager *PrehydrationManager
var prehydrationOnce sync.Once

func InitPrehydrationManager() {
	prehydrationOnce.Do(func() {
		GlobalPrehydrationManager = &PrehydrationManager{
			activePrehydrations: make(map[string]bool),
			stopCh:              make(chan struct{}),
			notifyCh:            make(chan struct{}, 10),
		}
		go GlobalPrehydrationManager.pollLoop()
		go GlobalPrehydrationManager.schedulerLoop()
	})
}

func (pm *PrehydrationManager) Stop() {
	if pm == nil {
		return
	}
	pm.mu.Lock()
	select {
	case <-pm.stopCh:
	default:
		close(pm.stopCh)
	}
	pm.mu.Unlock()
}

func (pm *PrehydrationManager) GetLatestCapacity() *ProviderCapacityReport {
	if pm == nil {
		return nil
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.latestCapacity
}

// AutoDiscoverDispatcharrURL extracts Dispatcharr's base URL from incoming /proxy/vod/... paths.
func (pm *PrehydrationManager) AutoDiscoverDispatcharrURL(targetURL string) {
	if pm == nil || targetURL == "" {
		return
	}
	u, err := url.Parse(targetURL)
	if err != nil {
		return
	}
	baseURL := fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	pm.mu.Lock()
	if pm.discoveredDispatcharrURL == "" {
		pm.discoveredDispatcharrURL = baseURL
		log.Printf("[Prehydration] Auto-discovered Dispatcharr base URL: %s", baseURL)
	}
	pm.mu.Unlock()
}

func (pm *PrehydrationManager) getEffectiveDispatcharrURL() string {
	if GlobalConfig != nil {
		explicit := strings.TrimSpace(GlobalConfig.GetDispatcharrURL())
		explicit = strings.Trim(explicit, "\"'`")
		explicit = strings.TrimRight(explicit, "/")
		if explicit != "" {
			return explicit
		}
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.discoveredDispatcharrURL
}

func (pm *PrehydrationManager) pollCapacityOnce() {
	baseURL := pm.getEffectiveDispatcharrURL()
	if baseURL == "" {
		return
	}

	pollURL := fmt.Sprintf("%s/plugins/stasharr/provider-capacity", baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		log.Printf("[Prehydration] Failed to create poll request for %s: %v", pollURL, err)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[Prehydration] Failed to poll Dispatcharr provider capacity at %s: %v", pollURL, err)
		pm.mu.Lock()
		pm.latestCapacity = &ProviderCapacityReport{
			Status:    "unavailable",
			Timestamp: time.Now().Unix(),
			Error:     err.Error(),
		}
		pm.mu.Unlock()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[Prehydration] Dispatcharr provider capacity at %s returned HTTP %d", pollURL, resp.StatusCode)
		pm.mu.Lock()
		pm.latestCapacity = &ProviderCapacityReport{
			Status:    "unavailable",
			Timestamp: time.Now().Unix(),
			Error:     fmt.Sprintf("HTTP %d from Stasharr", resp.StatusCode),
		}
		pm.mu.Unlock()
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[Prehydration] Failed to read capacity response from %s: %v", pollURL, err)
		return
	}

	var report ProviderCapacityReport
	if err := json.Unmarshal(body, &report); err != nil {
		log.Printf("[Prehydration] Failed to parse capacity JSON from %s: %v", pollURL, err)
		return
	}

	pm.mu.Lock()
	firstPoll := pm.latestCapacity == nil || pm.latestCapacity.Status != "ok"
	pm.latestCapacity = &report
	pm.lastPollTime = time.Now()
	pm.mu.Unlock()

	if firstPoll {
		log.Printf("[Prehydration] Connected to Dispatcharr provider capacity (%s): active=%d, available=%d, max=%d",
			pollURL, report.TotalActiveStreams, report.TotalAvailableStreams, report.TotalMaxStreams)
	}

	// Check if active prehydrations need to yield due to live streams taking reserved slots
	pm.checkYieldConditions()
}

func (pm *PrehydrationManager) pollLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Initial check on boot
	pm.pollCapacityOnce()

	for {
		interval := 5 * time.Second
		if GlobalConfig != nil {
			interval = GlobalConfig.GetDispatcharrPollInterval()
		}

		select {
		case <-pm.stopCh:
			return
		case <-ticker.C:
			ticker.Reset(interval)
			pm.pollCapacityOnce()
		}
	}
}

// CanStartPrehydration determines if spare capacity exists to launch a new prehydration worker.
// Requires available_streams >= PREHYDRATE_CAPACITY_RESERVATION + 1 so that after claiming 1 slot,
// at least PREHYDRATE_CAPACITY_RESERVATION slots remain free for live viewers.
func (pm *PrehydrationManager) CanStartPrehydration(targetURL string) bool {
	if GlobalConfig != nil && !GlobalConfig.GetEnableEpisodePrehydration() {
		return false
	}

	reservation := 1
	if GlobalConfig != nil {
		reservation = GlobalConfig.GetPrehydrateCapacityReservation()
	}

	pm.mu.RLock()
	report := pm.latestCapacity
	pm.mu.RUnlock()

	// If Stasharr capacity is unavailable or unconfigured, fallback to reactive mode:
	// allow 1 prehydration concurrently, yielding on 503
	if report == nil || report.Status != "ok" {
		pm.mu.RLock()
		activeCount := len(pm.activePrehydrations)
		pm.mu.RUnlock()
		return activeCount == 0
	}

	available := report.TotalAvailableStreams
	// If report is scoped or target content capacity exists:
	if report.TargetAvailableStreams > 0 {
		available = report.TargetAvailableStreams
	}

	// Starting threshold: available >= reservation + 1
	return available >= (reservation + 1)
}

// ShouldYieldPrehydration checks if live streams have encroached on the capacity reservation.
// Because the running prehydration worker is already counted in active_streams, available represents
// the remaining live headroom. Yield when available < reservation.
func (pm *PrehydrationManager) ShouldYieldPrehydration(targetURL string) bool {
	reservation := 1
	if GlobalConfig != nil {
		reservation = GlobalConfig.GetPrehydrateCapacityReservation()
	}

	pm.mu.RLock()
	report := pm.latestCapacity
	pm.mu.RUnlock()

	if report == nil || report.Status != "ok" {
		return false
	}

	available := report.TotalAvailableStreams
	if report.TargetAvailableStreams > 0 && report.TargetAvailableStreams < available {
		available = report.TargetAvailableStreams
	}

	return available < reservation
}

// checkYieldConditions is called on every capacity poll to yield active prehydration workers if live streams started.
func (pm *PrehydrationManager) checkYieldConditions() {
	reservation := 1
	if GlobalConfig != nil {
		reservation = GlobalConfig.GetPrehydrateCapacityReservation()
	}

	pm.mu.RLock()
	report := pm.latestCapacity
	if report == nil || report.Status != "ok" {
		pm.mu.RUnlock()
		return
	}
	available := report.TotalAvailableStreams
	pm.mu.RUnlock()

	if available >= reservation {
		return
	}

	// Capacity tight: yield running prehydrations
	GlobalRegistry.Lock.RLock()
	var prehydratingFiles []*FileCacheState
	for _, fs := range GlobalRegistry.Files {
		fs.Lock.Lock()
		if fs.IsPrehydrating && len(fs.Clients) == 0 && !fs.IsYielding && fs.WriterActive {
			prehydratingFiles = append(prehydratingFiles, fs)
		}
		fs.Lock.Unlock()
	}
	GlobalRegistry.Lock.RUnlock()

	for _, fs := range prehydratingFiles {
		log.Printf("[Prehydration] Provider capacity tight (Available: %d, Reserved: %d). Yielding prehydration: %s",
			available, reservation, fs.Key)
		pm.YieldState(fs, "Live stream capacity reservation")
	}
}

// YieldState safely suspends an active prehydration, closing its upstream connection while keeping .part data.
func (pm *PrehydrationManager) YieldState(state *FileCacheState, reason string) {
	if state == nil {
		return
	}

	state.Lock.Lock()
	cancel := state.Cancel
	state.Cancel = nil
	state.IsYielding = true
	backoff := 30 * time.Second
	if GlobalConfig != nil {
		backoff = GlobalConfig.GetPrehydrateYieldBackoff()
	}
	state.YieldCooldownUntil = time.Now().Add(backoff)
	state.WriterActive = false
	state.Lock.Unlock()

	state.SetUpstreamStatus("Yielding")

	if cancel != nil {
		cancel()
	}

	pm.mu.Lock()
	delete(pm.activePrehydrations, state.Key)
	pm.mu.Unlock()

	log.Printf("[Prehydration] File %s transitioned to Yielding (%s). Upstream socket closed; .part blocks preserved.",
		state.Key, reason)

	if GlobalLogBroker != nil {
		GlobalLogBroker.BroadcastStateUpdate()
	}
}

// ResumeState resumes a prehydration that was previously in Yielding state.
func (pm *PrehydrationManager) ResumeState(state *FileCacheState) {
	if state == nil {
		return
	}

	state.Lock.Lock()
	if !state.IsYielding || state.RangeSet.IsComplete(state.TotalSize) {
		state.Lock.Unlock()
		return
	}
	state.IsYielding = false
	state.IsPrehydrating = true
	state.Lock.Unlock()

	pm.mu.Lock()
	pm.activePrehydrations[state.Key] = true
	pm.mu.Unlock()

	log.Printf("[Prehydration] Resuming yielded file: %s at offset %d", state.Key, state.RangeSet.TotalBytes())
	header := make(http.Header)
	header.Set("User-Agent", GlobalConfig.GetUpstreamUserAgent())
	StartCoordinator(state, header)
}

// OnCacheHit is invoked immediately when a player requests an episode that is already 100% on disk.
func (pm *PrehydrationManager) OnCacheHit(currentURL string, nextURLs []string) {
	if pm == nil || len(nextURLs) == 0 {
		return
	}
	pm.AutoDiscoverDispatcharrURL(currentURL)
	pm.Enqueue(nextURLs[0], nextURLs[1:])
}

// OnHydrationCompleted is called when an upstream download reaches 100% and commits to disk.
func (pm *PrehydrationManager) OnHydrationCompleted(state *FileCacheState) {
	if pm == nil || state == nil {
		return
	}

	state.Lock.Lock()
	next := state.NextURLs
	key := state.Key
	state.Lock.Unlock()

	pm.mu.Lock()
	delete(pm.activePrehydrations, key)
	pm.mu.Unlock()

	if len(next) > 0 {
		log.Printf("[Prehydration] Hydration completed for %s. Scheduling next episode: %s", key, next[0])
		pm.Enqueue(next[0], next[1:])
	}
}

// OnDownshift is called when the last client disconnects from an incomplete file.
func (pm *PrehydrationManager) OnDownshift(state *FileCacheState) {
	if pm == nil || state == nil {
		return
	}

	state.Lock.Lock()
	state.IsPrehydrating = true
	key := state.Key
	targetURL := state.URL
	state.Lock.Unlock()

	log.Printf("[Prehydration] Client disconnected from %s. Downshifting to prehydration mode.", key)

	if !pm.CanStartPrehydration(targetURL) {
		pm.YieldState(state, "Capacity tight on client disconnect")
	} else {
		pm.mu.Lock()
		pm.activePrehydrations[key] = true
		pm.mu.Unlock()
		state.SetUpstreamStatus("Prehydrating")
	}
}

// Enqueue adds an upcoming episode URL to the prehydration queue.
func (pm *PrehydrationManager) Enqueue(targetURL string, nextURLs []string) {
	if pm == nil || targetURL == "" {
		return
	}

	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext
	cacheDir := GlobalConfig.GetCacheDir()
	cacheFilePath := filepath.Join(cacheDir, cacheFileName)

	// If already completed on disk, chain immediately to the next one!
	if FileExists(cacheFilePath) {
		log.Printf("[Prehydration] Episode %s is already cached on disk (Cache HIT). Chaining to next in sequence.", targetURL)
		if len(nextURLs) > 0 {
			pm.Enqueue(nextURLs[0], nextURLs[1:])
		}
		return
	}

	pm.mu.Lock()
	// Deduplicate against queue
	for _, task := range pm.queue {
		if task.TargetURL == targetURL {
			pm.mu.Unlock()
			return
		}
	}

	task := &PrehydrationTask{
		TargetURL:  targetURL,
		NextURLs:   nextURLs,
		EnqueuedAt: time.Now(),
	}
	pm.queue = append(pm.queue, task)
	log.Printf("[Prehydration] Enqueued prehydration task: %s (Queue length: %d)", targetURL, len(pm.queue))
	pm.mu.Unlock()

	pm.notify()
}

func (pm *PrehydrationManager) notify() {
	select {
	case pm.notifyCh <- struct{}{}:
	default:
	}
}

func (pm *PrehydrationManager) schedulerLoop() {
	for {
		select {
		case <-pm.stopCh:
			return
		case <-pm.notifyCh:
		case <-time.After(2 * time.Second):
		}

		pm.processNextTask()
	}
}

func (pm *PrehydrationManager) processNextTask() {
	// 1. First, check if any yielded states are ready to resume
	GlobalRegistry.Lock.RLock()
	var yieldedCandidates []*FileCacheState
	for _, fs := range GlobalRegistry.Files {
		fs.Lock.Lock()
		if fs.IsYielding && len(fs.Clients) == 0 && time.Now().After(fs.YieldCooldownUntil) {
			yieldedCandidates = append(yieldedCandidates, fs)
		}
		fs.Lock.Unlock()
	}
	GlobalRegistry.Lock.RUnlock()

	for _, fs := range yieldedCandidates {
		if pm.CanStartPrehydration(fs.URL) {
			pm.ResumeState(fs)
			return
		}
	}

	// 2. Pop next queued task if capacity permits
	pm.mu.Lock()
	if len(pm.queue) == 0 {
		pm.mu.Unlock()
		return
	}
	task := pm.queue[0]
	pm.mu.Unlock()

	if !pm.CanStartPrehydration(task.TargetURL) {
		return
	}

	pm.mu.Lock()
	if len(pm.queue) > 0 && pm.queue[0] == task {
		pm.queue = pm.queue[1:]
	}
	pm.mu.Unlock()

	pm.executePrehydration(task)
}

func (pm *PrehydrationManager) executePrehydration(task *PrehydrationTask) {
	targetURL := task.TargetURL
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext
	cacheDir := GlobalConfig.GetCacheDir()
	cacheFilePath := filepath.Join(cacheDir, cacheFileName)

	if FileExists(cacheFilePath) {
		if len(task.NextURLs) > 0 {
			pm.Enqueue(task.NextURLs[0], task.NextURLs[1:])
		}
		return
	}

	GlobalRegistry.Lock.Lock()
	state, exists := GlobalRegistry.Files[cacheFileName]
	if exists {
		GlobalRegistry.Lock.Unlock()
		state.Lock.Lock()
		if state.IsYielding {
			state.Lock.Unlock()
			pm.ResumeState(state)
			return
		}
		state.Lock.Unlock()
		return
	}
	GlobalRegistry.Lock.Unlock()

	// Probe upstream for headers
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	header := make(http.Header)
	header.Set("User-Agent", GlobalConfig.GetUpstreamUserAgent())

	log.Printf("[Prehydration] Probing upstream for upcoming episode: %s", targetURL)
	totalSize, contentType, etag, lastMod, _, err := probeUpstream(ctx, targetURL, header, nil)
	if err != nil {
		log.Printf("[Prehydration] Probe error for %s: %v. Placing into Yielding state without blocking live clients.", targetURL, err)
		// Do NOT record FailedProbe tombstone on prehydration failure!
		// Instead create state in Yielding mode so live clients can still stream or resume.
		partPath := filepath.Join(GlobalConfig.GetTmpDir(), cacheFileName+".part")
		metaPath := cacheFilePath + ".json"
		backoff := 30 * time.Second
		if GlobalConfig != nil {
			backoff = GlobalConfig.GetPrehydrateYieldBackoff()
		}

		GlobalRegistry.Lock.Lock()
		state = NewFileCacheState(cacheFileName, targetURL, cacheFilePath, partPath, metaPath, contentType, etag, lastMod, totalSize)
		state.IsPrehydrating = true
		state.IsYielding = true
		state.YieldCooldownUntil = time.Now().Add(backoff)
		state.NextURLs = task.NextURLs
		state.UpstreamStatus = "Yielding"
		GlobalRegistry.Files[cacheFileName] = state
		GlobalRegistry.Lock.Unlock()
		return
	}

	if contentType == "" || strings.HasPrefix(strings.ToLower(contentType), "text/html") || contentType == "application/octet-stream" {
		contentType = getMimeTypeByExt(ext)
	}

	GlobalRegistry.PreEvict(totalSize)

	partPath := filepath.Join(GlobalConfig.GetTmpDir(), cacheFileName+".part")
	metaPath := cacheFilePath + ".json"

	GlobalRegistry.Lock.Lock()
	state = NewFileCacheState(cacheFileName, targetURL, cacheFilePath, partPath, metaPath, contentType, etag, lastMod, totalSize)
	state.IsPrehydrating = true
	state.IsYielding = false
	state.NextURLs = task.NextURLs
	GlobalRegistry.Files[cacheFileName] = state
	GlobalRegistry.Lock.Unlock()

	pm.mu.Lock()
	pm.activePrehydrations[cacheFileName] = true
	pm.mu.Unlock()

	log.Printf("[Prehydration] Starting background prehydration worker for %s (1 sequential worker, no TailPrefetch)", cacheFileName)
	StartCoordinator(state, header)
}
