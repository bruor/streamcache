package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type SSEEvent struct {
	Type string
	Data string
}

type LogBroker struct {
	lock             sync.Mutex
	listeners        map[chan SSEEvent]bool
	stdout           io.Writer
	broadcasting     atomic.Int32
	broadcastPending atomic.Int32
}

var GlobalLogBroker *LogBroker
var ServerInstanceID = fmt.Sprintf("%d", time.Now().UnixNano())

func InitLogBroker() {
	GlobalLogBroker = &LogBroker{
		listeners: make(map[chan SSEEvent]bool),
		stdout:    os.Stderr, // Duplicate logs to Stderr
	}
	// Direct all Go log output to our broker
	log.SetOutput(GlobalLogBroker)
}

func (lb *LogBroker) Write(p []byte) (n int, err error) {
	// Write to console
	n, err = lb.stdout.Write(p)

	// Broadcast to active SSE listeners
	lb.lock.Lock()
	defer lb.lock.Unlock()
	evt := SSEEvent{
		Type: "log",
		Data: string(p),
	}
	for listener := range lb.listeners {
		select {
		case listener <- evt:
		default:
			// Client channel full, skip to avoid blocking
		}
	}
	return n, err
}

func (lb *LogBroker) Register(ch chan SSEEvent) {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	lb.listeners[ch] = true
}

func (lb *LogBroker) Unregister(ch chan SSEEvent) {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	delete(lb.listeners, ch)
}

func handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Ensure the connection is closeable / check if flusher is supported
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	eventChan := make(chan SSEEvent, 200)
	if GlobalLogBroker != nil {
		GlobalLogBroker.Register(eventChan)
		defer GlobalLogBroker.Unregister(eventChan)
	}

	// Send initial state
	sendStateUpdate(w, flusher)

	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()

	// Handle SSE Stream
	for {
		select {
		case evt := <-eventChan:
			// Format as SSE data
			_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, evt.Data)
			if err != nil {
				return
			}
			flusher.Flush()

		case <-timer.C:
			// Periodically send full state update
			err := sendStateUpdate(w, flusher)
			if err != nil {
				return
			}
			interval := 3 * time.Second
			if GlobalRegistry != nil && GlobalRegistry.HasActiveActivity() {
				interval = 250 * time.Millisecond
			}
			timer.Reset(interval)

		case <-r.Context().Done():
			// Client disconnected
			return
		}
	}
}

func (lb *LogBroker) BroadcastStateUpdate() {
	lb.lock.Lock()
	hasListeners := len(lb.listeners) > 0
	lb.lock.Unlock()
	if !hasListeners {
		return
	}

	lb.broadcastPending.Store(1)
	if !lb.broadcasting.CompareAndSwap(0, 1) {
		return
	}
	defer lb.broadcasting.Store(0)

	for lb.broadcastPending.Swap(0) == 1 {
		payload, err := buildStatePayload()
		if err != nil {
			return
		}

		evt := SSEEvent{
			Type: "state",
			Data: string(payload),
		}

		lb.lock.Lock()
		for listener := range lb.listeners {
			select {
			case listener <- evt:
			default:
			}
		}
		lb.lock.Unlock()
	}
}

func sendStateUpdate(w http.ResponseWriter, flusher http.Flusher) error {
	payload, err := buildStatePayload()
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(w, "event: state\ndata: %s\n\n", string(payload))
	if err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

type dashboardClientPos struct {
	ID         string `json:"id"`
	Offset     int64  `json:"offset"`
	RemoteAddr string `json:"remote_addr"`
	UserAgent  string `json:"user_agent"`
	Username   string `json:"username,omitempty"`
	IsStarving bool   `json:"is_starving"`
}

type dashboardFile struct {
	Key                 string               `json:"key"`
	URL                 string               `json:"url"`
	TotalSize           int64                `json:"total_size"`
	Ranges              [][]int64            `json:"ranges"`
	DownloadedBytes     int64                `json:"downloaded_bytes"`
	ActiveWriter        bool                 `json:"active_writer"`
	ActiveClients       int                  `json:"active_clients"`
	ActiveWorkers       int                  `json:"active_workers"`
	MaxWorkers          int                  `json:"max_workers"`
	StarvingQueue       []int64              `json:"starving_queue"`
	StarvingList        map[string]int64     `json:"starving_clients"`
	DownloadSpeed       int64                `json:"download_speed"`
	AvgDownloadSpeed    int64                `json:"avg_download_speed"`
	UpstreamStatus      string               `json:"upstream_status"`
	IsStalled           bool                 `json:"is_stalled"`
	StalledSeconds      int                  `json:"stalled_seconds"`
	ClientPositions     []dashboardClientPos `json:"client_positions"`
	ConnectionsLast30s  int                  `json:"connections_last_30s"`
	ObservedCooldown    float64              `json:"observed_cooldown"`
	UpstreamUserAgent   string               `json:"upstream_user_agent"`
	IsPrehydrating      bool                 `json:"is_prehydrating"`
	IsYielding          bool                 `json:"is_yielding"`
	QueuedPrehydrations int                  `json:"queued_prehydrations"`
	NextURLs            []string             `json:"next_urls,omitempty"`
	CommitProgress      *CommitProgress      `json:"commit_progress,omitempty"`
}

type dashboardProbeBuffer struct {
	Key            string   `json:"key"`
	URL            string   `json:"url"`
	TotalSize      int64    `json:"total_size"`
	RAMBytes       int64    `json:"ram_bytes"`
	UpstreamStatus string   `json:"upstream_status"`
	IdleSeconds    int      `json:"idle_seconds"`
	NextURLs       []string `json:"next_urls,omitempty"`
}

type dashboardStorageInfo struct {
	EffectiveBackend   string `json:"effective_backend"`
	TmpDir             string `json:"tmp_dir"`
	TmpUsedBytes       int64  `json:"tmp_used_bytes"`
	TmpObjects         int    `json:"tmp_objects"`
	TmpFileMaxBytes    int64  `json:"tmp_file_max_bytes"`
	TmpFreeBytes       uint64 `json:"tmp_free_bytes"`
	TmpMinFreeBytes    int64  `json:"tmp_min_free_bytes"`
	CacheEnabled       bool   `json:"cache_enabled"`
	CacheDir           string `json:"cache_dir"`
	CacheUsedBytes     int64  `json:"cache_used_bytes"`
	CacheObjects       int    `json:"cache_objects"`
	CacheMaxBytes      int64  `json:"cache_max_bytes"`
	CacheFreeBytes     uint64 `json:"cache_free_bytes"`
	CacheMinFreeBytes  int64  `json:"cache_min_free_bytes"`
	CacheHealthy       bool   `json:"cache_healthy"`
	S3Enabled          bool   `json:"s3_enabled"`
	S3Bucket           string `json:"s3_bucket,omitempty"`
	S3Endpoint         string `json:"s3_endpoint,omitempty"`
	S3UsedBytes        int64  `json:"s3_used_bytes"`
	S3Objects          int    `json:"s3_objects"`
	S3MaxBytes           int64   `json:"s3_max_bytes"`
	S3Healthy            bool    `json:"s3_healthy"`
	CacheMaxIdleAgeSec   int64   `json:"cache_max_idle_age_seconds"`
	CacheBytesDownloaded int64   `json:"cache_bytes_downloaded"`
	CacheBytesServed     int64   `json:"cache_bytes_served"`
	CacheBytesServedDisk int64   `json:"cache_bytes_served_disk"`
	CacheBytesServedS3   int64   `json:"cache_bytes_served_s3"`
	CacheRatio           float64 `json:"cache_ratio"`
	CacheHitRatioPct     float64 `json:"cache_hit_ratio_pct"`
	DemotedDiskToS3      int64   `json:"demoted_disk_to_s3"`
	DemotedDiskToS3Bytes int64   `json:"demoted_disk_to_s3_bytes"`
	EvictedCapacity      int64   `json:"evicted_capacity"`
	EvictedCapacityBytes int64   `json:"evicted_capacity_bytes"`
	EvictedTTL               int64   `json:"evicted_ttl"`
	EvictedTTLBytes          int64   `json:"evicted_ttl_bytes"`
	HybridDemoteThresholdPct int     `json:"hybrid_demote_threshold_pct,omitempty"`
	HybridEagerS3Backup      bool    `json:"hybrid_eager_s3_backup,omitempty"`
}

type dashboardSnapshot struct {
	StorageBackend    string                 `json:"storage_backend"`
	TotalCacheSize    int64                  `json:"total_cache_size_bytes"`
	MaxCacheSize      int64                  `json:"max_cache_size_bytes"`
	MaxObservedSpeed  int64                  `json:"max_observed_speed"`
	DiskFreeBytes     uint64                 `json:"disk_free_bytes"`
	MinDiskFreeBytes  int64                  `json:"min_disk_free_bytes"`
	Storage           dashboardStorageInfo   `json:"storage"`
	ActiveFiles       []dashboardFile        `json:"active_downloads"`
	ActiveSessions    []ClientSession        `json:"active_connections"`
	ActiveVODClients  int                    `json:"active_vod_clients"`
	ActiveVODUpstream int                    `json:"active_vod_upstream"`
	ActiveVODProbes   int                    `json:"active_vod_probes"`
	ProbeActive       bool                   `json:"probe_active"`
	ProbeBuffers      []dashboardProbeBuffer `json:"probe_buffers"`
	ProviderCapacity  interface{}            `json:"provider_capacity,omitempty"`
	NASHealthy        bool                   `json:"nas_healthy"`
	S3Healthy         bool                   `json:"s3_healthy"`
	NASCircuitOpen    bool                   `json:"nas_circuit_open"`
	S3CircuitOpen     bool                   `json:"s3_circuit_open"`
	NASLastError      string                 `json:"nas_last_error,omitempty"`
	S3LastError       string                 `json:"s3_last_error,omitempty"`
}

func (s dashboardSnapshot) toBaseMap() map[string]interface{} {
	data := map[string]interface{}{
		"storage_backend":        s.StorageBackend,
		"total_cache_size_bytes": s.TotalCacheSize,
		"max_cache_size_bytes":   s.MaxCacheSize,
		"disk_free_bytes":        s.DiskFreeBytes,
		"storage":                s.Storage,
		"active_downloads":       s.ActiveFiles,
		"active_connections":     s.ActiveSessions,
		"active_vod_clients":     s.ActiveVODClients,
		"active_vod_upstream":    s.ActiveVODUpstream,
		"active_vod_probes":      s.ActiveVODProbes,
		"probe_active":           s.ProbeActive,
		"probe_buffers":          s.ProbeBuffers,
		"nas_healthy":            s.NASHealthy,
		"s3_healthy":             s.S3Healthy,
		"nas_circuit_open":       s.NASCircuitOpen,
		"s3_circuit_open":        s.S3CircuitOpen,
	}
	if s.NASLastError != "" {
		data["nas_last_error"] = s.NASLastError
	}
	if s.S3LastError != "" {
		data["s3_last_error"] = s.S3LastError
	}
	if s.ProviderCapacity != nil {
		data["provider_capacity"] = s.ProviderCapacity
	}
	return data
}

func collectDashboardSnapshot() dashboardSnapshot {
	activeSessions := GlobalRegistry.GetActiveSessions()

	var files []*FileCacheState
	if GlobalRegistry != nil {
		GlobalRegistry.Lock.RLock()
		files = make([]*FileCacheState, 0, len(GlobalRegistry.Files))
		for _, fileState := range GlobalRegistry.Files {
			files = append(files, fileState)
		}
		GlobalRegistry.Lock.RUnlock()
	}

	var activeFiles []dashboardFile
	var probeBuffers []dashboardProbeBuffer
	activeVODUpstream := 0
	activeVODProbes := 0
	var tmpUsedBytes int64
	var tmpObjects int
	var unsyncedDL, unsyncedSV int64
	for _, fileState := range files {
		fileDL, fileSV := fileState.UnsyncedTransferBytes()
		unsyncedDL += fileDL
		unsyncedSV += fileSV
		fileState.Lock.Lock()
		if fileState.ChunkManager != nil {
			tmpUsedBytes += fileState.ChunkManager.GetInFlightBytes()
			tmpObjects++
		} else if fileState.DiskMaterialized {
			tmpObjects++
			if fileState.DiskRangeSet != nil {
				tmpUsedBytes += fileState.DiskRangeSet.TotalBytes()
			} else if fileState.RangeSet != nil {
				tmpUsedBytes += fileState.RangeSet.TotalBytes()
			}
		}
		if fileState.IsProbe {
			ramBytes := int64(0)
			if fileState.LiveBuffer != nil {
				ramBytes = fileState.LiveBuffer.GetCurBytes()
			}
			lastProg := atomic.LoadInt64(&fileState.LastProgressTimeNano)
			idleSec := 0
			if lastProg > 0 {
				idleSec = int(time.Since(time.Unix(0, lastProg)).Seconds())
			}
			probeBuffers = append(probeBuffers, dashboardProbeBuffer{
				Key:            fileState.Key,
				URL:            fileState.URL,
				TotalSize:      fileState.TotalSize,
				RAMBytes:       ramBytes,
				UpstreamStatus: fileState.UpstreamStatus,
				IdleSeconds:    idleSec,
				NextURLs:       append([]string(nil), fileState.NextURLs...),
			})
			fileActiveConns := 0
			for _, conn := range fileState.RecentConnections {
				if conn.Closed.IsZero() {
					fileActiveConns++
				}
			}
			activeVODUpstream += fileActiveConns
			activeVODProbes += fileActiveConns
			fileState.Lock.Unlock()
			continue
		}

		isDiskMat := fileState.DiskMaterialized
		isProbeFile := !isDiskMat && !fileState.IsPrehydrating && !fileState.IsYielding && fileState.UpstreamStatus != "Committing" && fileState.UpstreamStatus != "Pending Recompose"
		fileActiveConns := 0
		for _, conn := range fileState.RecentConnections {
			if conn.Closed.IsZero() {
				fileActiveConns++
			}
		}
		activeVODUpstream += fileActiveConns
		if isProbeFile {
			if fileActiveConns > 0 {
				activeVODProbes += fileActiveConns
			} else {
				activeVODProbes++
			}
		}
		k := fileState.Key
		downloaded := fileState.RangeSet.TotalBytes()
		isCompleted := fileState.RangeSet.IsComplete(fileState.TotalSize)

		if isCompleted && fileState.UpstreamStatus != "Committing" && fileState.UpstreamStatus != "Pending Recompose" {
			fileState.Lock.Unlock()
			continue
		}

		// Do not draw hydrations until we are sure that they aren't probes:
		// A file is drawn only if it has materialized on disk, is explicitly
		// prehydrating/yielding, or is committing.
		if isProbeFile {
			fileState.Lock.Unlock()
			continue
		}

		ranges := fileState.RangeSet.ToSlices()

		// Collect unique starvation offsets and per-client starving map
		starveSet := make(map[int64]bool)
		var starveQueue []int64
		starvingCopy := make(map[string]int64, len(fileState.Starving))
		for clientID, offset := range fileState.Starving {
			starvingCopy[clientID] = offset
			if !starveSet[offset] {
				starveSet[offset] = true
				starveQueue = append(starveQueue, offset)
			}
		}

		// Collect client playback positions for this file
		var clientPositions []dashboardClientPos
		for _, sess := range activeSessions {
			if sess.Key == k {
				clientPositions = append(clientPositions, dashboardClientPos{
					ID:         sess.ID,
					Offset:     sess.Offset,
					RemoteAddr: sess.RemoteAddr,
					UserAgent:  sess.UserAgent,
					Username:   sess.Username,
					IsStarving: sess.StarvingOffset != -1,
				})
			}
		}

		// Filter out connections that are both closed AND closed more than 30 seconds ago
		filteredConns := make(map[string]*ConnectionInfo)
		activeCount := 0
		closedInLast30sCount := 0
		cutoff := time.Now().Add(-30 * time.Second)
		for id, conn := range fileState.RecentConnections {
			if conn.Closed.IsZero() {
				filteredConns[id] = conn
				activeCount++
			} else if conn.Closed.After(cutoff) {
				filteredConns[id] = conn
				closedInLast30sCount++
			}
		}
		fileState.RecentConnections = filteredConns

		effectiveUA := fileState.UpstreamUserAgent
		if effectiveUA == "" {
			effectiveUA = GlobalConfig.GetUpstreamUserAgent()
		}

		status := fileState.UpstreamStatus
		if status == "" || status == "Idle" || status == "Throttled" {
			status = "Downloading"
		}

		var commitProgress *CommitProgress
		if status == "Committing" && fileState.ChunkManager != nil {
			commitProgress = fileState.ChunkManager.GetCommitProgress()
		}

		activeFiles = append(activeFiles, dashboardFile{
			Key:                 k,
			URL:                 fileState.URL,
			TotalSize:           fileState.TotalSize,
			Ranges:              ranges,
			DownloadedBytes:     downloaded,
			ActiveWriter:        fileState.WriterActive,
			ActiveClients:       len(fileState.Clients),
			ActiveWorkers:       activeCount,
			MaxWorkers:          GlobalConfig.GetMaxUpstreamWorkers(),
			StarvingQueue:       starveQueue,
			StarvingList:        starvingCopy,
			DownloadSpeed:       fileState.DownloadSpeed,
			AvgDownloadSpeed:    fileState.AvgDownloadSpeed,
			UpstreamStatus:      status,
			IsStalled:           fileState.ZeroSpeedSec >= 3,
			StalledSeconds:      fileState.ZeroSpeedSec,
			ClientPositions:     clientPositions,
			ConnectionsLast30s:  activeCount + closedInLast30sCount,
			ObservedCooldown:    fileState.ObservedCooldown,
			UpstreamUserAgent:   effectiveUA,
			IsPrehydrating:      fileState.IsPrehydrating,
			IsYielding:          fileState.IsYielding,
			QueuedPrehydrations: len(fileState.NextURLs),
			NextURLs:            append([]string(nil), fileState.NextURLs...),
			CommitProgress:      commitProgress,
		})
		fileState.Lock.Unlock()
	}

	// Sort active files from most complete to least complete (descending)
	sort.Slice(activeFiles, func(i, j int) bool {
		compI := 0.0
		if activeFiles[i].TotalSize > 0 {
			compI = float64(activeFiles[i].DownloadedBytes) / float64(activeFiles[i].TotalSize)
		}
		compJ := 0.0
		if activeFiles[j].TotalSize > 0 {
			compJ = float64(activeFiles[j].DownloadedBytes) / float64(activeFiles[j].TotalSize)
		}
		if compI == compJ {
			return activeFiles[i].Key < activeFiles[j].Key
		}
		return compI > compJ
	})

	// Sort active sessions by start time (oldest first)
	sort.Slice(activeSessions, func(i, j int) bool {
		return activeSessions[i].StartTime < activeSessions[j].StartTime
	})

	activeVODClients := 0
	for _, sess := range activeSessions {
		if sess.ActiveConns > 0 {
			activeVODClients++
		}
	}

	inFlightHeaderProbes := int(GlobalInFlightProbes.Load())
	activeVODProbes += inFlightHeaderProbes
	activeVODUpstream += inFlightHeaderProbes

	probeActive := activeVODProbes > 0
	if !probeActive && GlobalRegistry != nil {
		probeActive = GlobalRegistry.IsProbeActive()
	}

	healthSnap := StorageHealthSnapshot{NASHealthy: true, S3Healthy: true}
	if hm := getGlobalStorageHealth(); hm != nil {
		healthSnap = hm.Snapshot()
	}

	var diskObjects, s3Objects int
	var diskIdxBytes, s3UsedBytes int64
	var totalDL, totalSV, servedDisk, servedS3 int64
	var lifecycleStats CacheLifecycleStats
	if idx := getGlobalCacheIndex(); idx != nil {
		diskObjects, diskIdxBytes, s3Objects, s3UsedBytes, _ = idx.StatsByTier()
		totalDL, totalSV, servedDisk, servedS3 = idx.TransferStats()
		lifecycleStats = idx.LifecycleStats()
	}
	totalDL += unsyncedDL
	totalSV += unsyncedSV
	var cacheRatio float64
	if totalDL > 0 {
		cacheRatio = float64(totalSV) / float64(totalDL)
	}
	var hitRatioPct float64
	if totalSV > 0 && totalSV > totalDL {
		hitRatioPct = float64(totalSV-totalDL) / float64(totalSV) * 100.0
	}

	effBackend := GlobalConfig.EffectiveStorageBackend()
	topo := GetStorageTopology()
	tmpDir := GlobalConfig.GetTmpDir()
	cacheDir := GlobalConfig.GetCacheDir()
	if topo != nil {
		if topo.TmpDir != "" {
			tmpDir = topo.TmpDir
		}
		if topo.CacheDir != "" {
			cacheDir = topo.CacheDir
		}
	}
	cacheEnabled := effBackend == "disk" || effBackend == "hybrid"
	s3Enabled := effBackend == "s3" || effBackend == "hybrid"

	cacheUsedBytes := GlobalRegistry.GetTotalSize()
	if cacheUsedBytes == 0 && diskIdxBytes > 0 {
		cacheUsedBytes = diskIdxBytes
	}
	s3Cfg := GlobalConfig.GetS3Config()

	var hybridDemoteThresholdPct int
	var hybridEagerS3Backup bool
	if cacheEnabled && s3Enabled && GlobalConfig != nil {
		hybridDemoteThresholdPct = GlobalConfig.GetHybridDemoteThreshold()
		hybridEagerS3Backup = GlobalConfig.GetHybridEagerS3Backup()
	}

	storageInfo := dashboardStorageInfo{
		EffectiveBackend:   effBackend,
		TmpDir:             tmpDir,
		TmpUsedBytes:       tmpUsedBytes,
		TmpObjects:         tmpObjects,
		TmpFileMaxBytes:    GlobalConfig.GetTmpFileMaxBytes(),
		TmpFreeBytes:       atomic.LoadUint64(&GlobalTmpDiskFreeBytes),
		TmpMinFreeBytes:    EffectiveTmpMinDiskFreeBytes(GlobalConfig, topo),
		CacheEnabled:       cacheEnabled,
		CacheDir:           cacheDir,
		CacheUsedBytes:     cacheUsedBytes,
		CacheObjects:       diskObjects,
		CacheMaxBytes:      GlobalConfig.GetCacheMaxBytes(),
		CacheFreeBytes:     atomic.LoadUint64(&GlobalDiskFreeBytes),
		CacheMinFreeBytes:  EffectiveCacheMinDiskFreeBytes(GlobalConfig, topo),
		CacheHealthy:       healthSnap.NASHealthy,
		S3Enabled:          s3Enabled,
		S3Bucket:           s3Cfg.Bucket,
		S3Endpoint:         s3Cfg.Endpoint,
		S3UsedBytes:        s3UsedBytes,
		S3Objects:          s3Objects,
		S3MaxBytes:           s3Cfg.MaxBytes,
		S3Healthy:            healthSnap.S3Healthy,
		CacheMaxIdleAgeSec:   int64(GlobalConfig.GetCacheMaxIdleAge().Seconds()),
		CacheBytesDownloaded: totalDL,
		CacheBytesServed:     totalSV,
		CacheBytesServedDisk: servedDisk,
		CacheBytesServedS3:   servedS3,
		CacheRatio:           cacheRatio,
		CacheHitRatioPct:     hitRatioPct,
		DemotedDiskToS3:      lifecycleStats.DemotedDiskToS3,
		DemotedDiskToS3Bytes: lifecycleStats.DemotedDiskToS3Bytes,
		EvictedCapacity:      lifecycleStats.EvictedCapacity,
		EvictedCapacityBytes: lifecycleStats.EvictedCapacityBytes,
		EvictedTTL:               lifecycleStats.EvictedTTL,
		EvictedTTLBytes:          lifecycleStats.EvictedTTLBytes,
		HybridDemoteThresholdPct: hybridDemoteThresholdPct,
		HybridEagerS3Backup:      hybridEagerS3Backup,
	}

	snap := dashboardSnapshot{
		StorageBackend:    GlobalConfig.GetStorageBackend(),
		TotalCacheSize:    GlobalRegistry.GetTotalSize(),
		MaxCacheSize:      GlobalConfig.GetCacheMaxBytes(),
		MaxObservedSpeed:  GlobalRegistry.GetMaxObservedSpeed(),
		DiskFreeBytes:     atomic.LoadUint64(&GlobalDiskFreeBytes),
		MinDiskFreeBytes:  GlobalConfig.GetCacheMinDiskFreeBytes(),
		Storage:           storageInfo,
		ActiveFiles:       activeFiles,
		ActiveSessions:    activeSessions,
		ActiveVODClients:  activeVODClients,
		ActiveVODUpstream: activeVODUpstream,
		ActiveVODProbes:   activeVODProbes,
		ProbeActive:       probeActive,
		ProbeBuffers:      probeBuffers,
		NASHealthy:        healthSnap.NASHealthy,
		S3Healthy:         healthSnap.S3Healthy,
		NASCircuitOpen:    healthSnap.NASCircuitOpen,
		S3CircuitOpen:     healthSnap.S3CircuitOpen,
		NASLastError:      healthSnap.NASLastError,
		S3LastError:       healthSnap.S3LastError,
	}
	if GlobalPrehydrationManager != nil {
		if capReport := GlobalPrehydrationManager.GetLatestCapacity(); capReport != nil {
			snap.ProviderCapacity = capReport
		}
	}
	return snap
}

func buildStatePayload() ([]byte, error) {
	snap := collectDashboardSnapshot()
	data := snap.toBaseMap()
	data["server_instance_id"] = ServerInstanceID
	data["max_observed_speed"] = snap.MaxObservedSpeed
	data["min_disk_free_bytes"] = snap.MinDiskFreeBytes
	return json.Marshal(data)
}
