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
	w.Header().Set("Connection", "keep-alive")
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

func buildStatePayload() ([]byte, error) {
	activeSessions := GlobalRegistry.GetActiveSessions()

	GlobalRegistry.Lock.RLock()
	files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
	for _, fileState := range GlobalRegistry.Files {
		files = append(files, fileState)
	}
	GlobalRegistry.Lock.RUnlock()

	type sseClientPos struct {
		ID         string `json:"id"`
		Offset     int64  `json:"offset"`
		RemoteAddr string `json:"remote_addr"`
		UserAgent  string `json:"user_agent"`
		Username   string `json:"username,omitempty"`
		IsStarving bool   `json:"is_starving"`
	}

	type sseFile struct {
		Key                string         `json:"key"`
		URL                string         `json:"url"`
		TotalSize          int64          `json:"total_size"`
		Ranges             [][]int64      `json:"ranges"`
		DownloadedBytes    int64          `json:"downloaded_bytes"`
		ActiveWriter       bool           `json:"active_writer"`
		ActiveClients      int            `json:"active_clients"`
		ActiveWorkers      int            `json:"active_workers"`
		MaxWorkers         int            `json:"max_workers"`
		StarvingQueue      []int64        `json:"starving_queue"`
		DownloadSpeed      int64          `json:"download_speed"`
		AvgDownloadSpeed   int64          `json:"avg_download_speed"`
		UpstreamStatus     string         `json:"upstream_status"`
		IsStalled          bool           `json:"is_stalled"`
		StalledSeconds     int            `json:"stalled_seconds"`
		ClientPositions    []sseClientPos `json:"client_positions"`
		ConnectionsLast30s int            `json:"connections_last_30s"`
		ObservedCooldown   float64        `json:"observed_cooldown"`
		UpstreamUserAgent  string         `json:"upstream_user_agent"`
		IsPrehydrating     bool           `json:"is_prehydrating"`
		IsYielding         bool           `json:"is_yielding"`
		QueuedPrehydrations int           `json:"queued_prehydrations"`
		NextURLs           []string       `json:"next_urls,omitempty"`
	}

	type sseProbeBuffer struct {
		Key            string   `json:"key"`
		URL            string   `json:"url"`
		TotalSize      int64    `json:"total_size"`
		RAMBytes       int64    `json:"ram_bytes"`
		UpstreamStatus string   `json:"upstream_status"`
		IdleSeconds    int      `json:"idle_seconds"`
		NextURLs       []string `json:"next_urls,omitempty"`
	}

	var activeFiles []sseFile
	var probeBuffers []sseProbeBuffer
	activeVODUpstream := 0
	activeVODProbes := 0
	for _, fileState := range files {
		fileState.Lock.Lock()
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
			probeBuffers = append(probeBuffers, sseProbeBuffer{
				Key:            fileState.Key,
				URL:            fileState.URL,
				TotalSize:      fileState.TotalSize,
				RAMBytes:       ramBytes,
				UpstreamStatus: fileState.UpstreamStatus,
				IdleSeconds:    idleSec,
				NextURLs:       append([]string(nil), fileState.NextURLs...),
			})
			fileState.Lock.Unlock()
			continue
		}

		isDiskMat := fileState.DiskMaterialized
		isProbeFile := !isDiskMat && !fileState.IsPrehydrating && !fileState.IsYielding && fileState.UpstreamStatus != "Committing"
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

		if isCompleted && fileState.UpstreamStatus != "Committing" {
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

		// Collect unique starvation offsets
		starveSet := make(map[int64]bool)
		var starveQueue []int64
		for _, offset := range fileState.Starving {
			if !starveSet[offset] {
				starveSet[offset] = true
				starveQueue = append(starveQueue, offset)
			}
		}

		// Collect client playback positions for this file
		var clientPositions []sseClientPos
		for _, sess := range activeSessions {
			if sess.Key == k {
				clientPositions = append(clientPositions, sseClientPos{
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

		activeFiles = append(activeFiles, sseFile{
			Key:                k,
			URL:                fileState.URL,
			TotalSize:          fileState.TotalSize,
			Ranges:             ranges,
			DownloadedBytes:    downloaded,
			ActiveWriter:       fileState.WriterActive,
			ActiveClients:      len(fileState.Clients),
			ActiveWorkers:      activeCount,
			MaxWorkers:         GlobalConfig.GetMaxUpstreamWorkers(),
			StarvingQueue:      starveQueue,
			DownloadSpeed:      fileState.DownloadSpeed,
			AvgDownloadSpeed:   fileState.AvgDownloadSpeed,
			UpstreamStatus:     status,
			IsStalled:          fileState.ZeroSpeedSec >= 3,
			StalledSeconds:     fileState.ZeroSpeedSec,
			ClientPositions:    clientPositions,
			ConnectionsLast30s: activeCount + closedInLast30sCount,
			ObservedCooldown:   fileState.ObservedCooldown,
			UpstreamUserAgent:  effectiveUA,
			IsPrehydrating:     fileState.IsPrehydrating,
			IsYielding:         fileState.IsYielding,
			QueuedPrehydrations: len(fileState.NextURLs),
			NextURLs:           append([]string(nil), fileState.NextURLs...),
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

	freeSpace := atomic.LoadUint64(&GlobalDiskFreeBytes)

	data := map[string]interface{}{
		"server_instance_id":     ServerInstanceID,
		"total_cache_size_bytes": GlobalRegistry.GetTotalSize(),
		"max_cache_size_bytes":   GlobalConfig.GetCacheMaxBytes(),
		"max_observed_speed":     GlobalRegistry.GetMaxObservedSpeed(),
		"disk_free_bytes":        freeSpace,
		"min_disk_free_bytes":    GlobalConfig.GetMinDiskFreeBytes(),
		"active_downloads":       activeFiles,
		"active_connections":     activeSessions,
		"active_vod_clients":     activeVODClients,
		"active_vod_upstream":    activeVODUpstream,
		"active_vod_probes":      activeVODProbes,
		"probe_active":           probeActive,
		"probe_buffers":          probeBuffers,
	}

	if GlobalPrehydrationManager != nil {
		if capReport := GlobalPrehydrationManager.GetLatestCapacity(); capReport != nil {
			data["provider_capacity"] = capReport
		}
	}

	return json.Marshal(data)
}
