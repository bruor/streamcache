package main

import (
	"context"
	"encoding/json"
	"fmt"
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

type FileMeta struct {
	URL           string `json:"url"`
	TotalSize     int64  `json:"total_size"`
	ContentType   string `json:"content_type,omitempty"`
	ETag          string `json:"etag"`
	LastModified  string `json:"last_modified"`
	LastValidated int64  `json:"last_validated"` // Unix timestamp
	Completed     bool   `json:"completed"`
}

type ClientConnection struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	URL            string `json:"url"`
	RemoteAddr     string `json:"remote_addr"`
	UserAgent      string `json:"user_agent"`
	Username       string `json:"username,omitempty"`
	Offset         int64  `json:"offset"`
	StartOffset    int64  `json:"start_offset"`
	TotalSize      int64  `json:"total_size"`
	StartTime      int64  `json:"start_time"` // Unix timestamp
	InCache        bool   `json:"in_cache"`
	StarvingOffset int64  `json:"starving_offset"` // -1 if not starving
}

type ClientSession struct {
	ID             string                      `json:"id"`
	Key            string                      `json:"key"`
	URL            string                      `json:"url"`
	RemoteAddr     string                      `json:"remote_addr"`
	UserAgent      string                      `json:"user_agent"`
	Username       string                      `json:"username,omitempty"`
	Offset         int64                       `json:"offset"`
	MaxOffset      int64                       `json:"max_offset"`
	TotalSize      int64                       `json:"total_size"`
	StartTime      int64                       `json:"start_time"` // Unix timestamp
	LastActive     time.Time                   `json:"last_active"`
	InCache        bool                        `json:"in_cache"`
	StarvingOffset int64                       `json:"starving_offset"` // -1 if not starving
	ActiveConns    int                         `json:"active_conns"`
	Conns          map[string]*SessionConnInfo `json:"-"`
}

type SessionConnInfo struct {
	Opened time.Time
}

type FileCacheState struct {
	Key           string
	URL           string
	Path          string
	PartPath      string
	MetaPath      string
	TotalSize     int64
	RangeSet      *RangeSet
	Cond          *sync.Cond
	ContentType   string
	ETag          string
	LastModified  string
	LastValidated int64

	Lock               sync.Mutex
	Cancel             context.CancelFunc
	WriterActive       bool
	Clients            map[string]chan struct{}
	Starving           map[string]int64 // Map of clientID -> starving byte offset
	StarvingSince      map[string]time.Time // Map of clientID -> when starvation began
	File               *os.File         // Open handle to writing file
	NotifyChan         chan struct{}    // Channel to wake coordinator on client starvation
	BytesDownloadedSec int64
	TotalBytesDownloaded int64
	HydrationStartTime   int64
	DownloadSpeed        int64
	AvgDownloadSpeed     int64
	SmoothDownloadSpeed  float64
	ZeroSpeedSec         int
	UpstreamStatus       string
	UpstreamUserAgent    string
	RecentConnections    map[string]*ConnectionInfo
	ObservedCooldown     float64
	ActiveWorkers        []*Worker
	WorkersLock          sync.Mutex
	PendingTailPrefetchOffset int64 // -1 if none
	Sniffed              bool
	LiveBuffer           *LiveBuffer
	Pipeline             StorageSink
	ChunkManager         *S3ChunkManager
	IsPrehydrating       bool
	IsYielding           bool
	YieldCooldownUntil   time.Time
	NextURLs             []string
}

func NewFileCacheState(key, targetURL, cacheFilePath, partPath, metaPath, contentType, etag, lastMod string, totalSize int64) *FileCacheState {
	bufSize := int64(64 * 1024 * 1024)
	if GlobalConfig != nil {
		bufSize = GlobalConfig.GetWritebackBufferSize()
	}

	s := &FileCacheState{
		Key:                       key,
		URL:                       targetURL,
		Path:                      cacheFilePath,
		PartPath:                  partPath,
		MetaPath:                  metaPath,
		TotalSize:                 totalSize,
		RangeSet:                  NewRangeSet(),
		ContentType:               contentType,
		ETag:                      etag,
		LastModified:              lastMod,
		LastValidated:             time.Now().Unix(),
		Clients:                   make(map[string]chan struct{}),
		Starving:                  make(map[string]int64),
		StarvingSince:             make(map[string]time.Time),
		NotifyChan:                make(chan struct{}, 20),
		UpstreamStatus:            "Idle",
		PendingTailPrefetchOffset: -1,
		LiveBuffer:                NewLiveBuffer(bufSize),
	}
	if GlobalStorage != nil && GlobalStorage.Type() == "s3" {
		chunkSize := int64(64 * 1024 * 1024)
		if GlobalConfig != nil {
			chunkSize = GlobalConfig.GetS3Config().ChunkSize
		}
		s.ChunkManager = NewS3ChunkManager(s, GlobalStorage, chunkSize)
	}
	s.Cond = sync.NewCond(&s.Lock)
	return s
}

type ConnectionInfo struct {
	ID     string
	Opened time.Time
	Closed time.Time // IsZero if still active
}

func (s *FileCacheState) SetUpstreamStatus(status string) {
	targetStatus := status
	if status == "Connecting" || status == "Retrying" {
		s.WorkersLock.Lock()
		for _, w := range s.ActiveWorkers {
			if atomic.LoadInt64(&w.connBytesWritten) > 0 || atomic.LoadInt64(&w.speed) > 0 {
				targetStatus = "Downloading"
				break
			}
		}
		s.WorkersLock.Unlock()
	}

	s.Lock.Lock()
	defer s.Lock.Unlock()

	// Terminal states cannot be overridden by worker transitions
	if s.UpstreamStatus == "Committing" || s.UpstreamStatus == "Completed" || s.UpstreamStatus == "Cancelled" {
		if status == "Committing" || status == "Completed" || status == "Cancelled" {
			s.UpstreamStatus = status
			if GlobalLogBroker != nil {
				go GlobalLogBroker.BroadcastStateUpdate()
			}
		}
		return
	}

	if s.IsYielding && status != "Committing" && status != "Completed" && status != "Cancelled" {
		targetStatus = "Yielding"
	} else if s.IsPrehydrating && targetStatus == "Downloading" && len(s.Clients) == 0 {
		targetStatus = "Prehydrating"
	}

	if s.UpstreamStatus != targetStatus {
		s.UpstreamStatus = targetStatus
		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}
	}
}

func (s *FileCacheState) RecordConnStart(connID string) {
	s.Lock.Lock()
	defer s.Lock.Unlock()
	if s.RecentConnections == nil {
		s.RecentConnections = make(map[string]*ConnectionInfo)
	}
	s.RecentConnections[connID] = &ConnectionInfo{ID: connID, Opened: time.Now()}
}

func (s *FileCacheState) RecordConnEnd(connID string) {
	s.Lock.Lock()
	defer s.Lock.Unlock()
	if s.RecentConnections != nil {
		if conn, exists := s.RecentConnections[connID]; exists {
			conn.Closed = time.Now()
		}
	}
}

type CacheRegistry struct {
	Lock             sync.RWMutex
	Files            map[string]*FileCacheState
	Connections      map[string]*ClientConnection
	Sessions         map[string]*ClientSession
	FailedProbes     map[string]time.Time
	TotalCacheSize   int64
	MaxObservedSpeed int64
	Evicting         bool
}

var GlobalRegistry *CacheRegistry
var GlobalDiskFreeBytes uint64

func InitRegistry() {
	if GlobalStorage == nil {
		_ = InitStorage()
	}

	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
	}

	GlobalRegistry.LoadExistingCache()

	// Initial population of disk free space
	space, _ := GetDiskFreeSpace(GlobalConfig.GetCacheDir())
	atomic.StoreUint64(&GlobalDiskFreeBytes, space)
	go startDiskFreeMonitor()

	go GlobalRegistry.startSpeedMonitor()
}

func (r *CacheRegistry) HasActiveActivity() bool {
	if r == nil {
		return false
	}
	r.Lock.RLock()
	defer r.Lock.RUnlock()
	return len(r.Files) > 0 || len(r.Sessions) > 0
}

func (r *CacheRegistry) GetMaxObservedSpeed() int64 {
	if r == nil {
		return 0
	}
	r.Lock.RLock()
	defer r.Lock.RUnlock()
	return r.MaxObservedSpeed
}

func (r *CacheRegistry) RecordFailedProbe(key string, duration time.Duration) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.FailedProbes == nil {
		r.FailedProbes = make(map[string]time.Time)
	}
	r.FailedProbes[key] = time.Now().Add(duration)
}

func (r *CacheRegistry) ClearFailedProbe(key string) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.FailedProbes != nil {
		delete(r.FailedProbes, key)
	}
}

func (r *CacheRegistry) IsProbeFailed(key string) bool {
	r.Lock.RLock()
	defer r.Lock.RUnlock()
	if r.FailedProbes == nil {
		return false
	}
	expiry, exists := r.FailedProbes[key]
	if !exists {
		return false
	}
	return time.Now().Before(expiry)
}

func (r *CacheRegistry) SweepExpiredProbes() {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.FailedProbes == nil {
		return
	}
	now := time.Now()
	for key, expiry := range r.FailedProbes {
		if now.After(expiry) {
			delete(r.FailedProbes, key)
		}
	}
}

func startDiskFreeMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	for range ticker.C {
		space, err := GetDiskFreeSpace(GlobalConfig.GetCacheDir())
		if err == nil {
			atomic.StoreUint64(&GlobalDiskFreeBytes, space)
		}
		if GlobalRegistry != nil {
			GlobalRegistry.SweepExpiredProbes()
		}
	}
}

func (r *CacheRegistry) startSpeedMonitor() {
	ticker := time.NewTicker(1 * time.Second)
	for range ticker.C {
		now := time.Now().Unix()
		shouldBroadcast := false
		r.Lock.RLock()
		files := make([]*FileCacheState, 0, len(r.Files))
		for _, fileState := range r.Files {
			files = append(files, fileState)
		}
		r.Lock.RUnlock()

		// Check if any file is currently committing to prevent false-positive throttling during disk operations
		isAnyCommitting := false
		for _, fs := range files {
			fs.Lock.Lock()
			if fs.UpstreamStatus == "Committing" {
				isAnyCommitting = true
			}
			fs.Lock.Unlock()
		}

		var maxSpeedCandidate int64
		for _, fileState := range files {
			fileState.Lock.Lock()
			instantSpeed := float64(fileState.BytesDownloadedSec)
			fileState.BytesDownloadedSec = 0

			// Fast-Snap Speed Engine: instant drop to 0 and instant jump on start
			if instantSpeed == 0 {
				fileState.SmoothDownloadSpeed = 0
			} else if fileState.SmoothDownloadSpeed == 0 {
				fileState.SmoothDownloadSpeed = instantSpeed
			} else {
				fileState.SmoothDownloadSpeed = (0.5 * instantSpeed) + (0.5 * fileState.SmoothDownloadSpeed)
			}
			fileState.DownloadSpeed = int64(fileState.SmoothDownloadSpeed)

			if fileState.DownloadSpeed > maxSpeedCandidate {
				maxSpeedCandidate = fileState.DownloadSpeed
			}

			// Calculate overall average download speed
			if fileState.HydrationStartTime > 0 {
				elapsed := now - fileState.HydrationStartTime
				if elapsed < 1 {
					elapsed = 1
				}
				fileState.AvgDownloadSpeed = fileState.TotalBytesDownloaded / elapsed
			}

			// Zero-speed stall tracking
			if fileState.WriterActive && !isAnyCommitting && fileState.UpstreamStatus != "Committing" {
				if instantSpeed == 0 {
					fileState.ZeroSpeedSec++
					if fileState.ZeroSpeedSec == 3 {
						shouldBroadcast = true
					}
				} else {
					wasStalled := fileState.ZeroSpeedSec >= 3
					fileState.ZeroSpeedSec = 0
					if wasStalled {
						shouldBroadcast = true
					}
				}
			} else {
				fileState.ZeroSpeedSec = 0
			}

			fileState.Lock.Unlock()
		}

		if maxSpeedCandidate > 0 {
			r.Lock.Lock()
			if maxSpeedCandidate > r.MaxObservedSpeed {
				r.MaxObservedSpeed = maxSpeedCandidate
			}
			r.Lock.Unlock()
		}

		if shouldBroadcast && GlobalLogBroker != nil {
			GlobalLogBroker.BroadcastStateUpdate()
		}
	}
}

func getIPOnly(addr string) string {
	ip := addr
	if idx := strings.LastIndex(addr, ":"); idx != -1 {
		if bracketIdx := strings.LastIndex(addr, "]"); bracketIdx != -1 {
			if idx > bracketIdx {
				ip = addr[:idx]
			}
		} else if !strings.Contains(addr, "]") && strings.Count(addr, ":") == 1 {
			ip = addr[:idx]
		}
	}
	return strings.Trim(ip, "[]")
}

func isGenericUA(ua string) bool {
	uaLower := strings.ToLower(ua)
	generics := []string{"ffmpeg", "lavf", "vlc", "mpv", "curl", "wget", "libcurl", "stagefright", "unknown", "internal"}
	for _, g := range generics {
		if strings.Contains(uaLower, g) {
			return true
		}
	}
	return false
}

func userAgentsMatch(ua1, ua2 string) bool {
	if ua1 == ua2 {
		return true
	}
	return isGenericUA(ua1) && isGenericUA(ua2)
}

func parseRangeStart(rangeHeader string) int64 {
	if rangeHeader == "" {
		return 0
	}
	prefix := "bytes="
	if !strings.HasPrefix(rangeHeader, prefix) {
		return 0
	}
	s := rangeHeader[len(prefix):]
	if idx := strings.Index(s, "-"); idx != -1 {
		s = s[:idx]
	}
	s = strings.TrimSpace(s)
	var val int64
	_, err := fmt.Sscanf(s, "%d", &val)
	if err != nil {
		return 0
	}
	return val
}

func (r *CacheRegistry) RegisterConnection(connID string, conn *ClientConnection) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections == nil {
		r.Connections = make(map[string]*ClientConnection)
	}
	r.Connections[connID] = conn

	// Find a matching session
	now := time.Now()
	var matchedSession *ClientSession
	if r.Sessions != nil {
		var bestSession *ClientSession
		var bestDiff int64 = -1

		for _, session := range r.Sessions {
			if session.Key != conn.Key {
				continue
			}
			if getIPOnly(session.RemoteAddr) != getIPOnly(conn.RemoteAddr) {
				continue
			}
			if !userAgentsMatch(session.UserAgent, conn.UserAgent) {
				continue
			}

			// If the session is idle (0 active conns), it is a match candidate for player resume/seek
			if session.ActiveConns == 0 {
				bestSession = session
				break
			}

			// Check if connection is an initial startup probe (within 5s of session start, or tail probe before main stream)
			isNearEOF := conn.TotalSize > 0 && conn.Offset >= conn.TotalSize-15*1024*1024
			isStartupProbe := (now.Unix()-session.StartTime <= 5) || (isNearEOF && session.MaxOffset < conn.TotalSize-15*1024*1024)

			diff := conn.Offset - session.Offset
			if diff < 0 {
				diff = -diff
			}

			// Match to this session if it is an initial startup probe OR playback offset is close (within 15MB)
			if isStartupProbe || diff <= 15*1024*1024 {
				if bestDiff == -1 || diff < bestDiff {
					bestDiff = diff
					bestSession = session
				}
			}
		}
		matchedSession = bestSession
	}

	if matchedSession != nil {
		if matchedSession.Conns == nil {
			matchedSession.Conns = make(map[string]*SessionConnInfo)
		}
		matchedSession.Conns[connID] = &SessionConnInfo{
			Opened: time.Now(),
		}
		matchedSession.ActiveConns = len(matchedSession.Conns)
		matchedSession.LastActive = time.Now()
		matchedSession.Username = conn.Username

		isNearEOF := conn.TotalSize > 0 && conn.Offset >= conn.TotalSize-10*1024*1024
		wasNotPreceded := matchedSession.MaxOffset < conn.TotalSize-15*1024*1024
		readLimitNotExceeded := (conn.Offset - conn.StartOffset) <= 2*1024*1024
		isProbe := isNearEOF && wasNotPreceded && readLimitNotExceeded

		if !isProbe {
			matchedSession.Offset = conn.Offset
			if conn.Offset > matchedSession.MaxOffset {
				matchedSession.MaxOffset = conn.Offset
			}
		}
		// Keep InCache = false if ANY connection in this session required CDN download
		if !conn.InCache {
			matchedSession.InCache = false
		}
		matchedSession.StarvingOffset = conn.StarvingOffset
	} else {
		if r.Sessions == nil {
			r.Sessions = make(map[string]*ClientSession)
		}
		isNearEOF := conn.TotalSize > 0 && conn.Offset >= conn.TotalSize-10*1024*1024
		isProbe := isNearEOF && (conn.Offset-conn.StartOffset) <= 2*1024*1024

		var initialMaxOffset int64
		if !isProbe {
			initialMaxOffset = conn.Offset
		}

		newSession := &ClientSession{
			ID:             connID,
			Key:            conn.Key,
			URL:            conn.URL,
			RemoteAddr:     conn.RemoteAddr,
			UserAgent:      conn.UserAgent,
			Username:       conn.Username,
			Offset:         conn.Offset,
			MaxOffset:      initialMaxOffset,
			TotalSize:      conn.TotalSize,
			StartTime:      time.Now().Unix(),
			LastActive:     time.Now(),
			InCache:        conn.InCache,
			StarvingOffset: conn.StarvingOffset,
			ActiveConns:    1,
			Conns: map[string]*SessionConnInfo{
				connID: {Opened: time.Now()},
			},
		}
		r.Sessions[connID] = newSession
	}
}

func (r *CacheRegistry) UnregisterConnection(connID string) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		delete(r.Connections, connID)
	}
	if r.Sessions != nil {
		for _, session := range r.Sessions {
			if _, exists := session.Conns[connID]; exists {
				delete(session.Conns, connID)
				session.ActiveConns = len(session.Conns)
				session.LastActive = time.Now()
				break
			}
		}
	}
}

func (r *CacheRegistry) UpdateConnectionOffset(connID string, offset int64) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.Offset = offset
			if r.Sessions != nil {
				for _, session := range r.Sessions {
					if _, exists := session.Conns[connID]; exists {
						isNearEOF := conn.TotalSize > 0 && offset >= conn.TotalSize-10*1024*1024
						sessionIsNearEOF := session.TotalSize > 0 && session.Offset >= session.TotalSize-10*1024*1024

						if len(session.Conns) <= 1 || !isNearEOF || sessionIsNearEOF {
							session.Offset = offset
						}
						session.LastActive = time.Now()
						break
					}
				}
			}
		}
	}
}

func (r *CacheRegistry) SetConnectionInCache(connID string, inCache bool) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.InCache = inCache
			if r.Sessions != nil {
				for _, session := range r.Sessions {
					if _, exists := session.Conns[connID]; exists {
						if !inCache {
							session.InCache = false
						}
						session.LastActive = time.Now()
						break
					}
				}
			}
		}
	}
}

func (r *CacheRegistry) UpdateConnectionStarvation(connID string, starvingOffset int64) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.StarvingOffset = starvingOffset
			if r.Sessions != nil {
				for _, session := range r.Sessions {
					if _, exists := session.Conns[connID]; exists {
						session.StarvingOffset = starvingOffset
						session.LastActive = time.Now()
						break
					}
				}
			}
		}
	}
}

func (r *CacheRegistry) MarkConnectionsAsInCache(key string) {
	r.Lock.Lock()
	defer r.Lock.Unlock()
	// Do not mutate InCache for active connections/sessions so ongoing CDN playback sessions retain their [ CDN ] origin badge!
}

func (r *CacheRegistry) GetActiveSessions() []ClientSession {
	r.Lock.Lock()
	defer r.Lock.Unlock()

	now := time.Now()

	if r.Sessions != nil {
		for _, session := range r.Sessions {
			if len(session.Conns) > 1 {
				var oldestID string
				var oldestTime time.Time
				for id, info := range session.Conns {
					if oldestTime.IsZero() || info.Opened.Before(oldestTime) {
						oldestTime = info.Opened
						oldestID = id
					}
				}

				for id, info := range session.Conns {
					if id == oldestID {
						continue
					}
					if now.Sub(info.Opened) > 2*time.Second {
						if cc, ok := r.Connections[id]; ok {
							isNearEOF := cc.TotalSize > 0 && cc.Offset >= cc.TotalSize-10*1024*1024
							isProbe := isNearEOF && (cc.Offset-cc.StartOffset) <= 2*1024*1024
							var initialMaxOffset int64
							if !isProbe {
								initialMaxOffset = cc.Offset
							}

							newSession := &ClientSession{
								ID:             cc.ID,
								Key:            cc.Key,
								URL:            cc.URL,
								RemoteAddr:     cc.RemoteAddr,
								UserAgent:      cc.UserAgent,
								Username:       cc.Username,
								Offset:         cc.Offset,
								MaxOffset:      initialMaxOffset,
								TotalSize:      cc.TotalSize,
								StartTime:      cc.StartTime,
								LastActive:     now,
								InCache:        cc.InCache,
								StarvingOffset: cc.StarvingOffset,
								ActiveConns:    1,
								Conns: map[string]*SessionConnInfo{
									id: info,
								},
							}
							r.Sessions[id] = newSession
							delete(session.Conns, id)
						}
					}
				}
			}
		}

		// Calculate non-probe active connections count and set session offset/active conns
		for _, session := range r.Sessions {
			var oldestNonProbeOffset int64 = -1
			var oldestNonProbeTime time.Time
			nonProbeCount := 0

			for id, info := range session.Conns {
				cc, ok := r.Connections[id]
				if !ok {
					continue
				}
				isNearEOF := cc.TotalSize > 0 && cc.Offset >= cc.TotalSize-10*1024*1024
				wasNotPreceded := session.MaxOffset < cc.TotalSize-15*1024*1024
				readLimitNotExceeded := (cc.Offset - cc.StartOffset) <= 2*1024*1024
				isProbe := isNearEOF && wasNotPreceded && readLimitNotExceeded

				if !isProbe {
					nonProbeCount++
					if oldestNonProbeTime.IsZero() || info.Opened.Before(oldestNonProbeTime) {
						oldestNonProbeTime = info.Opened
						oldestNonProbeOffset = cc.Offset
					}
				}
			}

			if oldestNonProbeOffset != -1 {
				session.Offset = oldestNonProbeOffset
				if oldestNonProbeOffset > session.MaxOffset {
					session.MaxOffset = oldestNonProbeOffset
				}
			}
			session.ActiveConns = nonProbeCount
		}

		for id, session := range r.Sessions {
			if session.ActiveConns == 0 && now.Sub(session.LastActive) > 120*time.Second {
				delete(r.Sessions, id)
			}
		}
	}

	var list []ClientSession
	if r.Sessions != nil {
		for _, session := range r.Sessions {
			if session.ActiveConns > 0 || (now.Sub(session.LastActive) < 120*time.Second && session.MaxOffset > 0) {
				list = append(list, *session)
			}
		}
	}
	return list
}

// LoadExistingCache runs once at startup to calculate initial cache size
// and delete any dangling .part files that don't have metadata.
func (r *CacheRegistry) LoadExistingCache() {
	r.Lock.Lock()
	defer r.Lock.Unlock()

	cacheDir := GlobalConfig.GetCacheDir()
	tmpDir := GlobalConfig.GetTmpDir()

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

	atomic.StoreInt64(&r.TotalCacheSize, totalSize)
	log.Printf("[Registry] Scan complete. Initial Cache Size: %.2f GB", float64(totalSize)/(1024*1024*1024))
}

func (r *CacheRegistry) UpdateSize(delta int64) {
	atomic.AddInt64(&r.TotalCacheSize, delta)
}

func (r *CacheRegistry) GetTotalCacheSize() int64 {
	return atomic.LoadInt64(&r.TotalCacheSize)
}

func (r *CacheRegistry) PreEvict(incomingSizeBytes int64) {
	r.Lock.Lock()
	if r.Evicting {
		r.Lock.Unlock()
		return
	}

	currentSize := atomic.LoadInt64(&r.TotalCacheSize)
	cacheDir := GlobalConfig.GetCacheDir()

	// Calculate how much space we need to free based on cache limit if incoming file is added
	var cacheBytesToFree int64
	maxBytes := GlobalConfig.GetCacheMaxBytes()
	minDiskFree := GlobalConfig.GetMinDiskFreeBytes()
	if maxBytes > 0 && (currentSize+incomingSizeBytes) > maxBytes {
		cacheBytesToFree = (currentSize + incomingSizeBytes) - maxBytes
	}

	// Calculate how much space we need to free based on disk space limit
	var diskBytesToFree int64
	freeSpace, spaceErr := GetDiskFreeSpace(cacheDir)
	if spaceErr == nil && minDiskFree > 0 {
		requiredFree := uint64(incomingSizeBytes) + uint64(minDiskFree)
		if freeSpace < requiredFree {
			diskBytesToFree = int64(requiredFree - freeSpace)
		}
	}

	bytesToFree := cacheBytesToFree
	if diskBytesToFree > bytesToFree {
		bytesToFree = diskBytesToFree
	}

	if bytesToFree <= 0 {
		r.Lock.Unlock()
		return
	}

	// Fast pre-flight check: count total size of available inactive files
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		r.Lock.Unlock()
		return
	}

	var availableFreeable int64
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) == ".json" {
			continue
		}
		_, active := r.Files[entry.Name()]
		if !active {
			if info, err := entry.Info(); err == nil {
				availableFreeable += info.Size()
			}
		}
	}

	if availableFreeable < bytesToFree {
		log.Printf("[Evictor] WARNING: Cache pre-flight check required %.2f MB, but only %.2f MB of inactive files are available.",
			float64(bytesToFree)/(1024*1024), float64(availableFreeable)/(1024*1024))
	}

	log.Printf("[Evictor] Fast pre-flight check passed. Spawning background evictor for incoming file (%.2f MB). Cache size: %.2f GB (limit: %.2f GB), Free Disk Space: %.2f GB (min: %.2f GB). Need to free %.2f MB.",
		float64(incomingSizeBytes)/(1024*1024),
		float64(currentSize)/(1024*1024*1024), float64(maxBytes)/(1024*1024*1024),
		float64(freeSpace)/(1024*1024*1024), float64(minDiskFree)/(1024*1024*1024),
		float64(bytesToFree)/(1024*1024))

	r.Evicting = true
	r.Lock.Unlock()

	// Launch file deletions asynchronously in a background thread to prevent blocking HTTP handler/stream start
	go func() {
		defer func() {
			r.Lock.Lock()
			r.Evicting = false
			r.Lock.Unlock()
		}()
		r.evictInactiveFiles(bytesToFree)
	}()
}

func (r *CacheRegistry) evictInactiveFiles(bytesToFree int64) {
	cacheDir := GlobalConfig.GetCacheDir()

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

		atomic.AddInt64(&r.TotalCacheSize, -file.Size)

		freedBytes += file.Size
		evictedCount++

		log.Printf("[Evictor] Evicted cache file: %s (Size: %.2f MB, Last Access: %s)",
			file.Name, float64(file.Size)/(1024*1024), file.AccessTime.Format(time.RFC3339))
	}

	log.Printf("[Evictor] Eviction complete. Evicted %d objects, freed %.2f MB. New Cache Size: %.2f GB",
		evictedCount, float64(freedBytes)/(1024*1024), float64(r.getTotalSize())/(1024*1024*1024))
}

func (r *CacheRegistry) GetTotalSize() int64 {
	return atomic.LoadInt64(&r.TotalCacheSize)
}

func (r *CacheRegistry) getTotalSize() int64 {
	return r.GetTotalSize()
}
