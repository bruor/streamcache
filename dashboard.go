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
	"time"
)

type LogBroker struct {
	lock      sync.Mutex
	listeners map[chan string]bool
	stdout    io.Writer
}

var GlobalLogBroker *LogBroker

func InitLogBroker() {
	GlobalLogBroker = &LogBroker{
		listeners: make(map[chan string]bool),
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
	msg := string(p)
	for listener := range lb.listeners {
		select {
		case listener <- msg:
		default:
			// Client channel full, skip to avoid blocking
		}
	}
	return n, err
}

func (lb *LogBroker) Register(ch chan string) {
	lb.lock.Lock()
	defer lb.lock.Unlock()
	lb.listeners[ch] = true
}

func (lb *LogBroker) Unregister(ch chan string) {
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

	logChan := make(chan string, 100)
	GlobalLogBroker.Register(logChan)
	defer GlobalLogBroker.Unregister(logChan)

	// Send initial state
	sendStateUpdate(w, flusher)

	stateTicker := time.NewTicker(1 * time.Second)
	defer stateTicker.Stop()

	// Handle SSE Stream
	for {
		select {
		case logMsg := <-logChan:
			// Format as SSE data
			_, err := fmt.Fprintf(w, "event: log\ndata: %s\n\n", logMsg)
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

func sendStateUpdate(w http.ResponseWriter, flusher http.Flusher) error {
	GlobalRegistry.Lock.RLock()
	defer GlobalRegistry.Lock.RUnlock()

	type sseFile struct {
		Key           string   `json:"key"`
		URL           string   `json:"url"`
		TotalSize     int64    `json:"total_size"`
		TotalBlocks   int      `json:"total_blocks"`
		BlockStates   []int    `json:"block_states"`
		ActiveWriter  bool     `json:"active_writer"`
		ActiveClients int      `json:"active_clients"`
		StarvingQueue []int    `json:"starving_queue"`
	}

	var activeFiles []sseFile
	var activeConnections []ClientConnection
	for k, fileState := range GlobalRegistry.Files {
		fileState.Lock.Lock()

		// Deep copy block states array to prevent race conditions
		blocks := make([]int, len(fileState.BlockStates))
		copy(blocks, fileState.BlockStates)

		// Collect unique starvation blocks
		starveSet := make(map[int]bool)
		var starveQueue []int
		for _, block := range fileState.Starving {
			if !starveSet[block] {
				starveSet[block] = true
				starveQueue = append(starveQueue, block)
			}
		}

		activeFiles = append(activeFiles, sseFile{
			Key:           k,
			URL:           fileState.URL,
			TotalSize:     fileState.TotalSize,
			TotalBlocks:   fileState.TotalBlocks,
			BlockStates:   blocks,
			ActiveWriter:  fileState.WriterActive,
			ActiveClients: len(fileState.Clients),
			StarvingQueue: starveQueue,
		})
		fileState.Lock.Unlock()
	}

	for _, conn := range GlobalRegistry.Connections {
		activeConnections = append(activeConnections, *conn)
	}

	// Sort active files from most complete to least complete (descending)
	sort.Slice(activeFiles, func(i, j int) bool {
		compI := calculateCompleteness(activeFiles[i].BlockStates)
		compJ := calculateCompleteness(activeFiles[j].BlockStates)
		if compI == compJ {
			// Secondary sort: Key name alphabetically ascending
			return activeFiles[i].Key < activeFiles[j].Key
		}
		return compI > compJ
	})

	// Sort active connections by start time (oldest first)
	sort.Slice(activeConnections, func(i, j int) bool {
		return activeConnections[i].StartTime < activeConnections[j].StartTime
	})

	freeSpace, _ := GetDiskFreeSpace(GlobalConfig.CacheDir)

	data := map[string]interface{}{
		"total_cache_size_bytes": GlobalRegistry.TotalCacheSize,
		"max_cache_size_bytes":   GlobalConfig.CacheMaxBytes,
		"disk_free_bytes":        freeSpace,
		"active_downloads":       activeFiles,
		"active_connections":     activeConnections,
	}

	payload, err := json.Marshal(data)
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

func calculateCompleteness(blocks []int) float64 {
	if len(blocks) == 0 {
		return 0
	}
	completed := 0
	for _, status := range blocks {
		if status == BlockCompleted {
			completed++
		}
	}
	return float64(completed) / float64(len(blocks))
}
