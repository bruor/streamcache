package main

import (
	"encoding/json"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	BlockNotStarted  = 0
	BlockDownloading = 1
	BlockCompleted   = 2
)

type FileMeta struct {
	URL          string `json:"url"`
	TotalSize    int64  `json:"total_size"`
	ETag         string `json:"etag"`
	LastModified string `json:"last_modified"`
	LastValidated int64  `json:"last_validated"` // Unix timestamp
	Completed    bool   `json:"completed"`
	BlockStates  []int  `json:"block_states,omitempty"`
}

type ClientConnection struct {
	ID            string `json:"id"`
	Key           string `json:"key"`
	URL           string `json:"url"`
	RemoteAddr    string `json:"remote_addr"`
	UserAgent     string `json:"user_agent"`
	Offset        int64  `json:"offset"`
	TotalSize     int64  `json:"total_size"`
	StartTime     int64  `json:"start_time"` // Unix timestamp
	InCache       bool   `json:"in_cache"`
	StarvingBlock int    `json:"starving_block"` // -1 if not starving
}

type FileCacheState struct {
	Key          string
	URL          string
	Path         string
	PartPath     string
	MetaPath     string
	TotalSize    int64
	TotalBlocks  int
	BlockStates  []int
	ETag         string
	LastModified string
	LastValidated int64

	Lock         sync.Mutex
	WriterActive bool
	Clients      map[string]chan struct{}
	Starving     map[string]int // Map of clientID -> starving block index
	File         *os.File       // Open handle to writing file
	NotifyChan   chan struct{}  // Channel to wake coordinator on client starvation
	BytesDownloadedSec int64
	DownloadSpeed      int64
	UpstreamStatus     string
}

type CacheRegistry struct {
	Lock           sync.RWMutex
	Files          map[string]*FileCacheState
	Connections    map[string]*ClientConnection
	TotalCacheSize int64
	Evicting       bool
}

var GlobalRegistry *CacheRegistry
var GlobalDiskFreeBytes uint64

func InitRegistry() {
	GlobalRegistry = &CacheRegistry{
		Files:       make(map[string]*FileCacheState),
		Connections: make(map[string]*ClientConnection),
	}
	GlobalRegistry.LoadExistingCache()

	// Initial population of disk free space
	space, _ := GetDiskFreeSpace(GlobalConfig.CacheDir)
	atomic.StoreUint64(&GlobalDiskFreeBytes, space)
	go startDiskFreeMonitor()

	go GlobalRegistry.startSpeedMonitor()
}

func startDiskFreeMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		space, err := GetDiskFreeSpace(GlobalConfig.CacheDir)
		if err == nil {
			atomic.StoreUint64(&GlobalDiskFreeBytes, space)
		}
	}
}

func (r *CacheRegistry) startSpeedMonitor() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		r.Lock.RLock()
		for _, fileState := range r.Files {
			fileState.Lock.Lock()
			fileState.DownloadSpeed = fileState.BytesDownloadedSec
			fileState.BytesDownloadedSec = 0
			fileState.Lock.Unlock()
		}
		r.Lock.RUnlock()
	}
}

func (r *CacheRegistry) RegisterConnection(connID string, conn *ClientConnection) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections == nil {
		r.Connections = make(map[string]*ClientConnection)
	}
	r.Connections[connID] = conn
}

func (r *CacheRegistry) UnregisterConnection(connID string) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		delete(r.Connections, connID)
	}
}

func (r *CacheRegistry) UpdateConnectionOffset(connID string, offset int64) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.Offset = offset
		}
	}
}

func (r *CacheRegistry) SetConnectionInCache(connID string, inCache bool) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.InCache = inCache
		}
	}
}

func (r *CacheRegistry) UpdateConnectionStarvation(connID string, starvingBlock int) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.StarvingBlock = starvingBlock
		}
	}
}

// LoadExistingCache runs once at startup to calculate initial cache size
// and delete any dangling .part files that don't have metadata.
func (r *CacheRegistry) LoadExistingCache() {
	r.Lock.Lock()
	defer r.Lock.Unlock()

	cacheDir := GlobalConfig.CacheDir
	tmpDir := GlobalConfig.TmpDir

	// Ensure directories exist
	_ = os.MkdirAll(cacheDir, 0755)
	_ = os.MkdirAll(tmpDir, 0755)

	var totalSize int64

	// 1. Scan completed cache files and clean up orphaned metadata files
	_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			ext := filepath.Ext(path)
			if ext == ".json" {
				// Check if the corresponding media file exists
				mediaPath := strings.TrimSuffix(path, ".json")
				if _, err := os.Stat(mediaPath); os.IsNotExist(err) {
					_ = os.Remove(path)
				}
			} else {
				info, err := d.Info()
				if err == nil {
					totalSize += info.Size()
				}
			}
		}
		return nil
	})

	// 2. Clean up temporary directory on boot
	// Any unfinished .part files on boot are abandoned; we can remove them.
	files, err := os.ReadDir(tmpDir)
	if err == nil {
		for _, f := range files {
			_ = os.Remove(filepath.Join(tmpDir, f.Name()))
		}
	}

	r.TotalCacheSize = totalSize
	log.Printf("[Registry] Scan complete. Initial Cache Size: %.2f GB", float64(totalSize)/(1024*1024*1024))
}

func (r *CacheRegistry) UpdateSize(delta int64) {
	r.Lock.Lock()
	r.TotalCacheSize += delta
	currentSize := r.TotalCacheSize

	// Check if eviction is already running
	if r.Evicting {
		r.Lock.Unlock()
		return
	}

	// Quick check if eviction is potentially needed
	var cacheBytesToFree int64
	if GlobalConfig.CacheMaxBytes > 0 && currentSize > GlobalConfig.CacheMaxBytes {
		cacheBytesToFree = currentSize - GlobalConfig.CacheMaxBytes
	}

	// We also check if disk free space limit is enabled
	needEvictCheck := cacheBytesToFree > 0 || GlobalConfig.MinDiskFreeBytes > 0

	if !needEvictCheck {
		r.Lock.Unlock()
		return
	}

	r.Evicting = true
	r.Lock.Unlock()

	go r.runEviction()
}

func (r *CacheRegistry) runEviction() {
	defer func() {
		r.Lock.Lock()
		r.Evicting = false
		r.Lock.Unlock()
	}()

	r.Lock.RLock()
	currentSize := r.TotalCacheSize
	r.Lock.RUnlock()

	cacheDir := GlobalConfig.CacheDir

	// Calculate how much we need to free based on cache limit
	var cacheBytesToFree int64
	if GlobalConfig.CacheMaxBytes > 0 && currentSize > GlobalConfig.CacheMaxBytes {
		cacheBytesToFree = currentSize - GlobalConfig.CacheMaxBytes
	}

	// Calculate how much we need to free based on disk space limit
	var diskBytesToFree int64
	freeSpace, spaceErr := GetDiskFreeSpace(cacheDir)
	if spaceErr == nil && GlobalConfig.MinDiskFreeBytes > 0 {
		if freeSpace < uint64(GlobalConfig.MinDiskFreeBytes) {
			diskBytesToFree = int64(uint64(GlobalConfig.MinDiskFreeBytes) - freeSpace)
		}
	}

	bytesToFree := cacheBytesToFree
	if diskBytesToFree > bytesToFree {
		bytesToFree = diskBytesToFree
	}

	if bytesToFree <= 0 {
		return
	}

	log.Printf("[Evictor] Real-time eviction triggered. Cache size: %.2f GB (limit: %.2f GB), Free Disk Space: %.2f GB (min: %.2f GB). Need to free %.2f MB.",
		float64(currentSize)/(1024*1024*1024), float64(GlobalConfig.CacheMaxBytes)/(1024*1024*1024),
		float64(freeSpace)/(1024*1024*1024), float64(GlobalConfig.MinDiskFreeBytes)/(1024*1024*1024),
		float64(bytesToFree)/(1024*1024))

	// Collect completed files in cache directory
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		log.Printf("[Evictor] Error reading cache directory: %v", err)
		return
	}

	type fileInfo struct {
		Name       string
		Path       string
		Size       int64
		AccessTime time.Time
	}

	var files []fileInfo

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) == ".json" {
			continue
		}
		path := filepath.Join(cacheDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Try to read metadata JSON for last-accessed timestamp
		metaPath := path + ".json"
		var lastAccess time.Time
		metaBytes, err := os.ReadFile(metaPath)
		if err == nil {
			var meta FileMeta
			if json.Unmarshal(metaBytes, &meta) == nil && meta.LastValidated > 0 {
				lastAccess = time.Unix(meta.LastValidated, 0)
			}
		}

		if lastAccess.IsZero() {
			lastAccess = info.ModTime()
		}

		files = append(files, fileInfo{
			Name:       entry.Name(),
			Path:       path,
			Size:       info.Size(),
			AccessTime: lastAccess,
		})
	}

	// Sort files by access time ascending (oldest first)
	sort.Slice(files, func(i, j int) bool {
		return files[i].AccessTime.Before(files[j].AccessTime)
	})

	var freedBytes int64
	evictedCount := 0

	for _, file := range files {
		if freedBytes >= bytesToFree {
			break
		}

		// Prevent evicting files that are actively being downloaded or streamed
		r.Lock.RLock()
		_, active := r.Files[file.Name]
		r.Lock.RUnlock()

		if active {
			continue // skip active files
		}

		// Delete video and metadata JSON
		_ = os.Remove(file.Path)
		_ = os.Remove(file.Path + ".json")

		r.Lock.Lock()
		r.TotalCacheSize -= file.Size
		r.Lock.Unlock()

		freedBytes += file.Size
		evictedCount++

		log.Printf("[Evictor] Evicted cache file: %s (Size: %.2f MB, Last Access: %s)",
			file.Name, float64(file.Size)/(1024*1024), file.AccessTime.Format(time.RFC3339))
	}

	log.Printf("[Evictor] Eviction complete. Evicted %d objects, freed %.2f MB. New Cache Size: %.2f GB",
		evictedCount, float64(freedBytes)/(1024*1024), float64(r.getTotalSize())/(1024*1024*1024))
}

func (r *CacheRegistry) getTotalSize() int64 {
	r.Lock.RLock()
	defer r.Lock.RUnlock()
	return r.TotalCacheSize
}
