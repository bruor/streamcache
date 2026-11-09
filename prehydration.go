package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var zeroProgressSuspensionTimeout = 15 * time.Minute

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
	LiveClients            int              `json:"live_clients"`
	LiveStreams            *int             `json:"live_streams,omitempty"`
	VODStreams             *int             `json:"vod_streams,omitempty"`
	VODClients             *int             `json:"vod_clients,omitempty"`
	Providers              []ProviderDetail `json:"providers"`
	Error                  string           `json:"error,omitempty"`
}

type PrehydrationTask struct {
	TargetURL     string
	NextURLs      []string
	EnqueuedAt    time.Time
	NotBefore     time.Time
	ProbeAttempts int
}

type PrehydrationManager struct {
	mu                                sync.RWMutex
	latestCapacity                    *ProviderCapacityReport
	lastPollTime                      time.Time
	discoveredDispatcharrURL          string
	queue                             []*PrehydrationTask
	activePrehydrations               map[string]bool // Set of canonical cacheFileNames actively running prehydration
	prehydrationsStartedSinceLastPoll int             // Tracks prehydrations started/resumed since the last capacity poll
	stopCh                            chan struct{}
	notifyCh                          chan struct{}
	pollCh                            chan struct{}
}

var GlobalPrehydrationManager *PrehydrationManager
var prehydrationOnce sync.Once

func InitPrehydrationManager() {
	prehydrationOnce.Do(func() {
		GlobalPrehydrationManager = &PrehydrationManager{
			activePrehydrations: make(map[string]bool),
			stopCh:              make(chan struct{}),
			notifyCh:            make(chan struct{}, 10),
			pollCh:              make(chan struct{}, 1),
		}
		go GlobalPrehydrationManager.pollLoop()
		go GlobalPrehydrationManager.schedulerLoop()
	})
}

func (pm *PrehydrationManager) TriggerCapacityPoll() {
	if pm == nil || pm.pollCh == nil {
		return
	}
	select {
	case pm.pollCh <- struct{}{}:
	default:
	}
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
// If an explicit DISPATCHARR_URL is already configured, auto-discovery is skipped.
func (pm *PrehydrationManager) AutoDiscoverDispatcharrURL(targetURL string) {
	if pm == nil || targetURL == "" {
		return
	}
	if GlobalConfig != nil && strings.TrimSpace(GlobalConfig.GetDispatcharrURL()) != "" {
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
		explicit := strings.TrimRight(strings.TrimSpace(GlobalConfig.GetDispatcharrURL()), "/")
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
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
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
		return
	}

	var report ProviderCapacityReport
	if err := json.Unmarshal(body, &report); err != nil {
		return
	}

	pm.mu.Lock()
	pm.latestCapacity = &report
	pm.lastPollTime = time.Now()
	pm.prehydrationsStartedSinceLastPoll = 0
	pm.mu.Unlock()

	// Check if active prehydrations need to yield due to live streams taking reserved slots
	pm.checkYieldConditions()

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
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
		case <-pm.pollCh:
			select {
			case <-pm.stopCh:
				return
			case <-time.After(150 * time.Millisecond):
			}
			select {
			case <-pm.pollCh:
			default:
			}
			pm.pollCapacityOnce()
		case <-ticker.C:
			ticker.Reset(interval)
			pm.pollCapacityOnce()
		}
	}
}

// CanStartPrehydration determines if spare capacity exists to launch a new prehydration worker.
// Requires effective available streams >= PREHYDRATE_CAPACITY_RESERVATION + 1 so that after claiming 1 slot,
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
	startedSincePoll := pm.prehydrationsStartedSinceLastPoll
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

	effectiveAvailable := available - startedSincePoll
	if effectiveAvailable < 0 {
		effectiveAvailable = 0
	}

	// Starting threshold: effectiveAvailable >= reservation + 1
	return effectiveAvailable >= (reservation + 1)
}

// ShouldYieldPrehydration checks if live streams have encroached on the capacity reservation.
// Because the running prehydration worker is already counted in active_streams, available represents
// the remaining live headroom. Yield when available < reservation AND active human playback exists.
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

	if available >= reservation {
		return false
	}

	// Check if active playback sessions exist (Live TV/Timeshift in Dispatcharr or active VOD clients in Streamcache)
	hasActivePlayback := false
	if report.LiveStreams == nil {
		// Legacy / mock report where live stream breakdown was not provided
		hasActivePlayback = true
	} else if *report.LiveStreams > 0 || report.LiveClients > 0 {
		hasActivePlayback = true
	}

	if !hasActivePlayback && GlobalRegistry != nil {
		GlobalRegistry.Lock.RLock()
		for _, fs := range GlobalRegistry.Files {
			fs.Lock.Lock()
			if len(fs.Clients) > 0 {
				hasActivePlayback = true
			}
			fs.Lock.Unlock()
			if hasActivePlayback {
				break
			}
		}
		GlobalRegistry.Lock.RUnlock()
	}

	return hasActivePlayback
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
	liveStreams := 0
	hasExplicitLiveStreams := false
	if report.LiveStreams != nil {
		liveStreams = *report.LiveStreams
		hasExplicitLiveStreams = true
	}
	liveClients := report.LiveClients
	pm.mu.RUnlock()

	if available >= reservation {
		return
	}

	// Capacity tight: check for active playback sessions (Live TV or active VOD clients)
	GlobalRegistry.Lock.RLock()
	files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
	for _, fs := range GlobalRegistry.Files {
		files = append(files, fs)
	}
	GlobalRegistry.Lock.RUnlock()

	hasActiveVODPlayback := false
	var prehydratingFiles []*FileCacheState
	for _, fs := range files {
		fs.Lock.Lock()
		if len(fs.Clients) > 0 {
			hasActiveVODPlayback = true
		} else if fs.IsPrehydrating && len(fs.Clients) == 0 && !fs.IsYielding && fs.WriterActive {
			prehydratingFiles = append(prehydratingFiles, fs)
		}
		fs.Lock.Unlock()
	}

	// Only yield if real human playback is competing for capacity:
	// If live streams/clients are active, or an active VOD client is playing in Streamcache,
	// or if the capacity report does not specify live breakdown (legacy/fallback).
	hasActivePlayback := (!hasExplicitLiveStreams) || liveStreams > 0 || liveClients > 0 || hasActiveVODPlayback
	if !hasActivePlayback {
		// All active streams belong to background prehydrations or retrying workers.
		// Never yield prehydrations to themselves!
		return
	}

	// Yield Prioritization:
	// 1. Files that haven't received bytes in the longest time yield first (LastProgressTimeNano ASC)
	// 2. Tie-breaker: lowest completion % yields first
	sort.Slice(prehydratingFiles, func(i, j int) bool {
		progI := atomic.LoadInt64(&prehydratingFiles[i].LastProgressTimeNano)
		progJ := atomic.LoadInt64(&prehydratingFiles[j].LastProgressTimeNano)

		diff := progI - progJ
		if diff < -int64(3*time.Second) {
			return true // i has older progress -> yields first
		}
		if diff > int64(3*time.Second) {
			return false // j has older progress -> yields first
		}

		var pctI, pctJ float64
		if prehydratingFiles[i].TotalSize > 0 {
			pctI = float64(prehydratingFiles[i].RangeSet.TotalBytes()) / float64(prehydratingFiles[i].TotalSize)
		}
		if prehydratingFiles[j].TotalSize > 0 {
			pctJ = float64(prehydratingFiles[j].RangeSet.TotalBytes()) / float64(prehydratingFiles[j].TotalSize)
		}
		return pctI < pctJ
	})

	neededToYield := reservation - available
	yieldedCount := 0
	for _, fs := range prehydratingFiles {
		if yieldedCount >= neededToYield {
			break
		}
		log.Printf("[Prehydration] Provider capacity tight (Available: %d, Reserved: %d, LiveStreams: %d, ActiveVOD: %v). Yielding prehydration: %s",
			available, reservation, liveStreams, hasActiveVODPlayback, fs.Key)
		pm.YieldState(fs, "Live stream capacity reservation")
		yieldedCount++
	}
}

// checkZeroProgressSuspension inspects active prehydrations that have 0 connected clients.
// If an active prehydration has made 0 net-new media bytes progress for >= zeroProgressSuspensionTimeout (default 15m):
// 1. Suspend upstream coordinator/workers (state.WriterActive = false) to free provider connections.
// 2. Preserve .part file on disk for future playback deduplication & sector healing.
// 3. Pass the chaining baton: if state.NextURLs has queued episodes, pop NextURLs[0],
//    enqueue it for prehydration, and clear state.NextURLs so bingewatching continues!
func (pm *PrehydrationManager) checkZeroProgressSuspension() {
	GlobalRegistry.Lock.RLock()
	files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
	for _, fs := range GlobalRegistry.Files {
		files = append(files, fs)
	}
	GlobalRegistry.Lock.RUnlock()

	timeout := zeroProgressSuspensionTimeout
	nowNano := time.Now().UnixNano()

	for _, fs := range files {
		fs.Lock.Lock()
		if fs.IsProbe {
			if len(fs.Clients) > 0 {
				fs.Lock.Unlock()
				continue
			}
			lastProg := atomic.LoadInt64(&fs.LastProgressTimeNano)
			if lastProg == 0 {
				atomic.StoreInt64(&fs.LastProgressTimeNano, nowNano)
				fs.Lock.Unlock()
				continue
			}
			stallDuration := time.Duration(nowNano - lastProg)
			if stallDuration < timeout {
				fs.Lock.Unlock()
				continue
			}

			// Probe buffer expired after 15m without playback!
			log.Printf("[Probe] Probe buffer %s made ZERO progress for %s (>= %s). Evicting probe and cancelling chain.",
				fs.Key, stallDuration.Round(time.Second), timeout)

			fs.NextURLs = nil
			key := fs.Key
			cancel := fs.Cancel
			fs.Cancel = nil
			fs.WriterActive = false
			if fs.LiveBuffer != nil {
				fs.LiveBuffer.Clear()
				fs.LiveBuffer = nil
			}
			partPath := fs.PartPath
			metaPath := fs.MetaPath
			fs.Lock.Unlock()

			if cancel != nil {
				cancel()
			}
			_ = os.Remove(partPath)
			_ = os.Remove(metaPath)

			if GlobalRegistry != nil {
				GlobalRegistry.UnregisterProbeBuffer(key)
				GlobalRegistry.Lock.Lock()
				if existingState, exists := GlobalRegistry.Files[key]; exists && existingState == fs {
					delete(GlobalRegistry.Files, key)
				}
				GlobalRegistry.Lock.Unlock()
			}

			if GlobalLogBroker != nil {
				go GlobalLogBroker.BroadcastStateUpdate()
			}
			continue
		}

		if !fs.IsPrehydrating || len(fs.Clients) > 0 || fs.IsYielding || !fs.WriterActive {
			fs.Lock.Unlock()
			continue
		}

		lastProg := atomic.LoadInt64(&fs.LastProgressTimeNano)
		if lastProg == 0 {
			atomic.StoreInt64(&fs.LastProgressTimeNano, nowNano)
			fs.Lock.Unlock()
			continue
		}

		stallDuration := time.Duration(nowNano - lastProg)
		if stallDuration < timeout {
			fs.Lock.Unlock()
			continue
		}

		log.Printf("[Prehydration] Prehydration %s made ZERO net-new byte progress for %s (>= %s). Suspending upstream workers; preserving .part on disk.",
			fs.Key, stallDuration.Round(time.Second), timeout)

		var nextURL string
		var remainingNextURLs []string
		if len(fs.NextURLs) > 0 {
			nextURL = fs.NextURLs[0]
			if len(fs.NextURLs) > 1 {
				remainingNextURLs = append([]string{}, fs.NextURLs[1:]...)
			}
			fs.NextURLs = nil
		}
		fs.Lock.Unlock()

		pm.YieldState(fs, "15-minute zero progress inactivity suspension")

		if nextURL != "" {
			log.Printf("[Prehydration] Passing baton from stalled episode %s to next episode: %s (remaining chain: %d)",
				fs.Key, nextURL, len(remainingNextURLs))
			pm.Enqueue(nextURL, remainingNextURLs)
		}
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

	// Progressive backoff for rapid channel surfing:
	// If re-yielded within 2x base backoff (60s), increase backoff multiplier
	if !state.LastYieldTime.IsZero() && time.Since(state.LastYieldTime) < 2*backoff {
		state.YieldCount++
	} else {
		state.YieldCount = 1
	}
	state.LastYieldTime = time.Now()

	multiplier := 1 << (state.YieldCount - 1)
	if multiplier > 4 {
		multiplier = 4 // Max 4x backoff (e.g. 30s -> 60s -> 120s max)
	}
	actualBackoff := backoff * time.Duration(multiplier)
	state.YieldCooldownUntil = time.Now().Add(actualBackoff)
	state.WriterActive = false
	state.Lock.Unlock()

	state.SetUpstreamStatus("Yielding")

	if cancel != nil {
		cancel()
	}

	pm.mu.Lock()
	delete(pm.activePrehydrations, state.Key)
	if pm.prehydrationsStartedSinceLastPoll > 0 {
		pm.prehydrationsStartedSinceLastPoll--
	}
	pm.mu.Unlock()

	log.Printf("[Prehydration] File %s transitioned to Yielding (cooldown %s until %s, attempt #%d, %s). Upstream socket closed; .part blocks preserved.",
		state.Key, actualBackoff, state.YieldCooldownUntil.Format(time.RFC3339), state.YieldCount, reason)

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
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
	pm.prehydrationsStartedSinceLastPoll++
	pm.mu.Unlock()

	log.Printf("[Prehydration] Resuming yielded file: %s at offset %d", state.Key, state.RangeSet.TotalBytes())
	header := make(http.Header)
	header.Set("User-Agent", GlobalConfig.GetUpstreamUserAgent())
	StartCoordinator(state, header)
}

// OnPriorityUpgrade removes a file from active prehydrations when upgraded to live playback.
func (pm *PrehydrationManager) OnPriorityUpgrade(key string) {
	if pm == nil || key == "" {
		return
	}
	pm.mu.Lock()
	if pm.activePrehydrations[key] {
		delete(pm.activePrehydrations, key)
		if pm.prehydrationsStartedSinceLastPoll > 0 {
			pm.prehydrationsStartedSinceLastPoll--
		}
	}
	pm.mu.Unlock()
}

// OnCancelled removes a file from active prehydrations and releases its slot if manually cancelled.
func (pm *PrehydrationManager) OnCancelled(key string) {
	if pm == nil || key == "" {
		return
	}
	pm.mu.Lock()
	if pm.activePrehydrations[key] {
		delete(pm.activePrehydrations, key)
		if pm.prehydrationsStartedSinceLastPoll > 0 {
			pm.prehydrationsStartedSinceLastPoll--
		}
	}
	pm.mu.Unlock()
}

// OnPlayerPlayback is called when a human viewer begins playing an asset.
// It removes the played asset from the prehydration queue and clears NextURLs
// on all other files to ensure only the active playing title controls the downstream binge chain.
func (pm *PrehydrationManager) OnPlayerPlayback(targetURL string, nextURLs []string) {
	if pm == nil {
		return
	}
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext

	pm.mu.Lock()
	if pm.activePrehydrations[cacheFileName] {
		delete(pm.activePrehydrations, cacheFileName)
		if pm.prehydrationsStartedSinceLastPoll > 0 {
			pm.prehydrationsStartedSinceLastPoll--
		}
	}
	for i := len(pm.queue) - 1; i >= 0; i-- {
		if pm.queue[i].TargetURL == targetURL {
			pm.queue = append(pm.queue[:i], pm.queue[i+1:]...)
		}
	}
	pm.mu.Unlock()

	if len(nextURLs) > 0 {
		GlobalRegistry.Lock.RLock()
		files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
		for _, fs := range GlobalRegistry.Files {
			if fs.URL != targetURL {
				files = append(files, fs)
			}
		}
		GlobalRegistry.Lock.RUnlock()

		for _, fs := range files {
			fs.Lock.Lock()
			fs.NextURLs = nil
			fs.Lock.Unlock()
		}
	}
}

// OnCacheHit is invoked immediately when a player requests an episode that is already 100% on disk.
func (pm *PrehydrationManager) OnCacheHit(currentURL string, nextURLs []string) {
	if pm == nil || len(nextURLs) == 0 {
		return
	}
	pm.OnPlayerPlayback(currentURL, nextURLs)
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
	if pm.prehydrationsStartedSinceLastPoll > 0 {
		pm.prehydrationsStartedSinceLastPoll--
	}
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

	if !state.IsDiskMaterialized() {
		if GlobalRegistry != nil && state.TotalSize > 0 {
			GlobalRegistry.PreEvict(state.TotalSize)
		}
		_ = state.MaterializeDiskFile()
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
		pm.prehydrationsStartedSinceLastPoll++
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
	// 0. Check if any active prehydration file has stalled for 15 minutes without net-new bytes
	pm.checkZeroProgressSuspension()

	// 1. First, check if any yielded states are ready to resume
	GlobalRegistry.Lock.RLock()
	files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
	for _, fs := range GlobalRegistry.Files {
		files = append(files, fs)
	}
	GlobalRegistry.Lock.RUnlock()

	var yieldedCandidates []*FileCacheState
	for _, fs := range files {
		fs.Lock.Lock()
		if !fs.IsProbe && fs.IsYielding && len(fs.Clients) == 0 && time.Now().After(fs.YieldCooldownUntil) {
			yieldedCandidates = append(yieldedCandidates, fs)
		}
		fs.Lock.Unlock()
	}

	// Sort yielded candidates by completion percentage descending (highest completion resumes first)
	sort.Slice(yieldedCandidates, func(i, j int) bool {
		var pctI, pctJ float64
		if yieldedCandidates[i].TotalSize > 0 {
			pctI = float64(yieldedCandidates[i].RangeSet.TotalBytes()) / float64(yieldedCandidates[i].TotalSize)
		}
		if yieldedCandidates[j].TotalSize > 0 {
			pctJ = float64(yieldedCandidates[j].RangeSet.TotalBytes()) / float64(yieldedCandidates[j].TotalSize)
		}
		return pctI > pctJ
	})

	for _, fs := range yieldedCandidates {
		if !pm.CanStartPrehydration(fs.URL) {
			break
		}
		pm.ResumeState(fs)
	}

	// 2. Pop next queued tasks as long as capacity permits
	for {
		now := time.Now()
		pm.mu.Lock()
		readyIdx := -1
		for i, t := range pm.queue {
			if t.NotBefore.IsZero() || now.After(t.NotBefore) {
				readyIdx = i
				break
			}
		}
		if readyIdx == -1 {
			pm.mu.Unlock()
			break
		}
		task := pm.queue[readyIdx]
		pm.mu.Unlock()

		if !pm.CanStartPrehydration(task.TargetURL) {
			break
		}

		pm.mu.Lock()
		removed := false
		for i, t := range pm.queue {
			if t == task {
				pm.queue = append(pm.queue[:i], pm.queue[i+1:]...)
				removed = true
				break
			}
		}
		pm.mu.Unlock()

		if !removed {
			continue
		}

		pm.executePrehydration(task)
	}
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

	// Tentatively record prehydration slot to protect provider capacity during probe
	pm.mu.Lock()
	pm.prehydrationsStartedSinceLastPoll++
	pm.mu.Unlock()

	// Probe upstream for headers (with 5s timeout to avoid tying up workers if provider is at capacity)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	header := make(http.Header)
	header.Set("User-Agent", GlobalConfig.GetUpstreamUserAgent())

	log.Printf("[Prehydration] Probing upstream for upcoming episode: %s", targetURL)
	totalSize, contentType, etag, lastMod, _, err := probeUpstream(ctx, targetURL, header, nil)
	if err != nil || totalSize <= 0 {
		pm.mu.Lock()
		if pm.prehydrationsStartedSinceLastPoll > 0 {
			pm.prehydrationsStartedSinceLastPoll--
		}
		pm.mu.Unlock()

		log.Printf("[Prehydration] Probe error for %s (size=%d, err=%v). Deferring without registering hydration card or blocking live clients.", targetURL, totalSize, err)
		// Do NOT record FailedProbe tombstone and do NOT register a TotalSize=0 card in GlobalRegistry.Files.
		// Re-queue transient probe failures with backoff so they retry quietly when capacity/upstream recovers.
		if !isPermanentProbeError(err) && task.ProbeAttempts < 5 {
			backoff := 30 * time.Second
			if GlobalConfig != nil {
				backoff = GlobalConfig.GetPrehydrateYieldBackoff()
			}
			task.ProbeAttempts++
			task.NotBefore = time.Now().Add(backoff)
			pm.mu.Lock()
			alreadyQueued := false
			for _, t := range pm.queue {
				if t.TargetURL == targetURL {
					alreadyQueued = true
					break
				}
			}
			if !alreadyQueued {
				pm.queue = append(pm.queue, task)
			}
			pm.mu.Unlock()
		}
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

	log.Printf("[Prehydration] Starting background prehydration worker for %s (dynamic capacity pacing, no TailPrefetch)", cacheFileName)
	StartCoordinator(state, header)
}
