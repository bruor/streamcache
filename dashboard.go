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
	lock      sync.Mutex
	listeners map[chan SSEEvent]bool
	stdout    io.Writer
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

type RangeUpdateEvent struct {
	Key            string `json:"key"`
	Start          int64  `json:"start"`
	End            int64  `json:"end"`
	UpstreamStatus string `json:"upstream_status,omitempty"`
}

func (lb *LogBroker) BroadcastRangeUpdate(key string, start, end int64) {
	if start >= end {
		return
	}
	evtData, err := json.Marshal(RangeUpdateEvent{
		Key:   key,
		Start: start,
		End:   end,
	})
	if err != nil {
		return
	}

	evt := SSEEvent{
		Type: "range_update",
		Data: string(evtData),
	}

	lb.lock.Lock()
	defer lb.lock.Unlock()
	for listener := range lb.listeners {
		select {
		case listener <- evt:
		default:
		}
	}
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
	GlobalLogBroker.Register(eventChan)
	defer GlobalLogBroker.Unregister(eventChan)

	// Send initial state
	sendStateUpdate(w, flusher)

	stateTicker := time.NewTicker(5 * time.Second)
	defer stateTicker.Stop()

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

		case <-stateTicker.C:
			// Periodically send full state update
			err := sendStateUpdate(w, flusher)
			if err != nil {
				return
			}

		case <-r.Context().Done():
			// Client disconnected
			return
		}
	}
}

func (lb *LogBroker) BroadcastStateUpdate() {
	payload, err := buildStatePayload()
	if err != nil {
		return
	}

	evt := SSEEvent{
		Type: "state",
		Data: string(payload),
	}

	lb.lock.Lock()
	defer lb.lock.Unlock()
	for listener := range lb.listeners {
		select {
		case listener <- evt:
		default:
		}
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

	type sseFile struct {
		Key                string    `json:"key"`
		URL                string    `json:"url"`
		TotalSize          int64     `json:"total_size"`
		Ranges             [][]int64 `json:"ranges"`
		DownloadedBytes    int64     `json:"downloaded_bytes"`
		ActiveWriter       bool      `json:"active_writer"`
		ActiveClients      int       `json:"active_clients"`
		StarvingQueue      []int64   `json:"starving_queue"`
		DownloadSpeed      int64     `json:"download_speed"`
		AvgDownloadSpeed   int64     `json:"avg_download_speed"`
		UpstreamStatus     string    `json:"upstream_status"`
		ConnectionsLast30s int       `json:"connections_last_30s"`
		ObservedCooldown   float64   `json:"observed_cooldown"`
		UpstreamUserAgent  string    `json:"upstream_user_agent"`
	}

	var activeFiles []sseFile
	for _, fileState := range files {
		fileState.Lock.Lock()
		k := fileState.Key
		downloaded := fileState.RangeSet.TotalBytes()
		isCompleted := fileState.RangeSet.IsComplete(fileState.TotalSize)

		if isCompleted && fileState.UpstreamStatus != "Committing" {
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

		activeFiles = append(activeFiles, sseFile{
			Key:                k,
			URL:                fileState.URL,
			TotalSize:          fileState.TotalSize,
			Ranges:             ranges,
			DownloadedBytes:    downloaded,
			ActiveWriter:       fileState.WriterActive,
			ActiveClients:      len(fileState.Clients),
			StarvingQueue:      starveQueue,
			DownloadSpeed:      fileState.DownloadSpeed,
			AvgDownloadSpeed:   fileState.AvgDownloadSpeed,
			UpstreamStatus:     fileState.UpstreamStatus,
			ConnectionsLast30s: activeCount + closedInLast30sCount,
			ObservedCooldown:   fileState.ObservedCooldown,
			UpstreamUserAgent:  effectiveUA,
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

	freeSpace := atomic.LoadUint64(&GlobalDiskFreeBytes)

	data := map[string]interface{}{
		"server_instance_id":     ServerInstanceID,
		"total_cache_size_bytes": GlobalRegistry.GetTotalSize(),
		"max_cache_size_bytes":   GlobalConfig.GetCacheMaxBytes(),
		"disk_free_bytes":        freeSpace,
		"min_disk_free_bytes":    GlobalConfig.GetMinDiskFreeBytes(),
		"active_downloads":       activeFiles,
		"active_connections":     activeSessions,
	}

	return json.Marshal(data)
}
