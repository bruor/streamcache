package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"math"
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
	CacheSource    string `json:"cache_source,omitempty"`
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
	CacheSource    string                      `json:"cache_source,omitempty"`
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
	DiskRangeSet  *RangeSet
	Cond          *sync.Cond
	ContentType   string
	ETag          string
	LastModified  string
	LastValidated int64

	Lock               sync.Mutex
	Cancel             context.CancelFunc
	WriterActive       bool
	IsCommitting       bool
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
	ChunkManager         *ScratchChunkManager
	TargetTier           StorageTier // Explicit storage tier routing ("disk", "s3", "probe")
	UseScratchManager    bool        // True when staged chunks use ScratchChunkManager hole-punching
	AccountedCacheBytes  int64       // Bytes contributed to GlobalRegistry.TotalCacheSize (TierDisk only)
	IsPrehydrating       bool
	IsYielding           bool
	IsProbe              bool
	YieldCooldownUntil   time.Time
	YieldCount           int
	LastYieldTime        time.Time
	NextURLs             []string
	DiskMaterialized     bool
	LastProgressTimeNano int64            // Unix nano timestamp of last net-new unique byte added to RangeSet (atomic)
	StalledGaps          map[int64]int64  // Map of [corruptStartOffset]readableGoodOffset for zero-fill synthesis
	coordMu              sync.Mutex
	coordGen             uint64
}

// NewFileCacheState constructs a FileCacheState and evaluates L1 disk admission via GlobalRegistry.
// Callers must NOT hold GlobalRegistry.Lock when calling this function.
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
		DiskRangeSet:              NewRangeSet(),
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
		LastProgressTimeNano:      time.Now().UnixNano(),
		StalledGaps:               make(map[int64]int64),
	}
	targetTier := TierDisk
	useScratch := false

	mode := "disk"
	if GlobalStorage != nil && (GlobalStorage.Type() == "s3" || GlobalStorage.Type() == "hybrid") {
		mode = GlobalStorage.Type()
	} else if GlobalConfig != nil {
		mode = GlobalConfig.EffectiveStorageBackend()
	} else if GlobalStorage != nil && GlobalStorage.Type() == "tmp" {
		mode = "tmp"
	}

	switch mode {
	case "s3":
		targetTier = TierS3
		useScratch = true
		s3Target, chunkSize := resolveS3TargetBackend()
		s.ChunkManager = NewScratchChunkManager(s, s3Target, chunkSize)
	case "tmp":
		targetTier = TierTmp
		useScratch = false
	case "hybrid":
		admit := true
		reason := ""
		if GlobalRegistry != nil {
			admit, reason = GlobalRegistry.CanAdmitToHybridL1(totalSize)
		}
		if admit {
			targetTier = TierDisk
			s.TargetTier = targetTier
			s.configureNASStagingTarget()
			useScratch = s.UseScratchManager
		} else {
			s3Target, chunkSize := resolveS3TargetBackend()
			if s3Target == nil {
				targetTier = TierTmp
				useScratch = false
			} else {
				targetTier = TierS3
				useScratch = true
				s.ChunkManager = NewScratchChunkManager(s, s3Target, chunkSize)
			}
			log.Printf("[Admission] Bypassing hybrid L1 for %s (%d bytes -> %s): %s", key, totalSize, targetTier, reason)
		}
	default:
		admit := true
		reason := ""
		if GlobalRegistry != nil {
			admit, reason = GlobalRegistry.CanAdmitToDiskL1(totalSize)
		}
		if !admit {
			targetTier = TierTmp
			useScratch = false
			log.Printf("[Admission] Bypassing L1 disk cache for %s (%d bytes -> TierTmp): %s", key, totalSize, reason)
		} else {
			targetTier = TierDisk
			s.TargetTier = targetTier
			s.configureNASStagingTarget()
			useScratch = s.UseScratchManager
		}
	}

	s.TargetTier = targetTier
	s.UseScratchManager = useScratch
	s.Cond = sync.NewCond(&s.Lock)
	return s
}

func resolveS3TargetBackend() (StorageBackend, int64) {
	s3Target := GlobalStorage
	if tb, ok := GlobalStorage.(*TieredStorageBackend); ok && tb.S3() != nil {
		s3Target = tb.S3()
	}
	chunkSize := int64(64 * 1024 * 1024)
	if s3b, ok := s3Target.(*S3StorageBackend); ok && s3b.ChunkSize() > 0 {
		chunkSize = s3b.ChunkSize()
	} else if GlobalConfig != nil {
		chunkSize = GlobalConfig.GetS3Config().ChunkSize
	}
	return s3Target, chunkSize
}

func (s *FileCacheState) configureNASStagingTarget() {
	topo := GetStorageTopology()
	cacheDir := ""
	if GlobalConfig != nil {
		cacheDir = GlobalConfig.GetCacheDir()
	}
	if topo == nil || !topo.CrossFSStaging || cacheDir == "" {
		return
	}
	s.UseScratchManager = true
	chunkSize := int64(16 * 1024 * 1024)
	if GlobalConfig != nil {
		if s3cs := GlobalConfig.GetS3Config().ChunkSize; s3cs > 0 && s3cs < chunkSize {
			chunkSize = s3cs
		}
		if maxTmp := GlobalConfig.GetTmpFileMaxBytes(); maxTmp > 0 && chunkSize > maxTmp/2 {
			if half := maxTmp / 2; half > 0 {
				chunkSize = half
			}
		}
	}
	nasTarget := NewNASStagingTarget(cacheDir, s.Key, topo.SequentialNASStage)
	s.ChunkManager = NewScratchChunkManagerWithTarget(s, nasTarget, chunkSize)
	log.Printf("[Admission] Admitted %s (%d bytes) to TierDisk with cross-FS NAS staging (sequential=%v, punchHole=%v)",
		s.Key, s.TotalSize, topo.SequentialNASStage, topo.TmpPunchHoleSupported)
}

func (s *FileCacheState) targetTierLocked() StorageTier {
	if s.TargetTier != "" {
		return s.TargetTier
	}
	if s.ChunkManager != nil {
		if s.ChunkManager.TargetName() == "nas" {
			return TierDisk
		}
		return TierS3
	}
	if GlobalStorage != nil {
		switch GlobalStorage.Type() {
		case "s3":
			return TierS3
		case "tmp":
			return TierTmp
		}
	}
	return TierDisk
}

// GetTargetTier safely returns the explicit target storage tier for this state.
func (s *FileCacheState) GetTargetTier() StorageTier {
	if s == nil {
		return TierDisk
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	return s.targetTierLocked()
}

// UsesScratchManager safely reports whether this state uses ScratchChunkManager staging.
func (s *FileCacheState) UsesScratchManager() bool {
	if s == nil {
		return false
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	return s.UseScratchManager || s.ChunkManager != nil
}

// MaxDeadZoneSkip is the absolute maximum byte span the reader is allowed to zero-fill (16MB).
// Any unreadable or missing span larger than 16MB is treated as an error, never synthesized as zeroes.
const MaxDeadZoneSkip = 16 * 1024 * 1024

// CheckStalledGap checks if offset falls within any recorded dead zone [gapStart, gapEnd).
// Returns the gapEnd offset if true, or 0 if not stalled or if the gap exceeds MaxDeadZoneSkip.
func (s *FileCacheState) CheckStalledGap(offset int64) int64 {
	s.Lock.Lock()
	defer s.Lock.Unlock()
	for gStart, gEnd := range s.StalledGaps {
		if offset >= gStart && offset < gEnd {
			if (gEnd - gStart) <= MaxDeadZoneSkip {
				return gEnd
			}
		}
	}
	return 0
}

// RecordStalledGap records an unreadable sector [start, end), strictly capped at MaxDeadZoneSkip.
func (s *FileCacheState) RecordStalledGap(start, end int64) {
	if start >= end || (end-start) > MaxDeadZoneSkip {
		return
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	if s.StalledGaps == nil {
		s.StalledGaps = make(map[int64]int64)
	}
	s.StalledGaps[start] = end
}

type ConnectionInfo struct {
	ID     string
	Opened time.Time
	Closed time.Time // IsZero if still active
}

// GetChunkManager safely reads s.ChunkManager under s.Lock.
func (s *FileCacheState) GetChunkManager() *ScratchChunkManager {
	if s == nil {
		return nil
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	return s.ChunkManager
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

	if s.IsProbe && len(s.Clients) == 0 {
		targetStatus = "Probed (Idle)"
	} else if s.IsYielding && status != "Committing" && status != "Completed" && status != "Cancelled" {
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
	if s.RecentConnections == nil {
		s.RecentConnections = make(map[string]*ConnectionInfo)
	}
	s.RecentConnections[connID] = &ConnectionInfo{ID: connID, Opened: time.Now()}
	s.Lock.Unlock()

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
	if GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.TriggerCapacityPoll()
	}
}

func (s *FileCacheState) RecordConnEnd(connID string) {
	s.Lock.Lock()
	if s.RecentConnections != nil {
		if conn, exists := s.RecentConnections[connID]; exists {
			conn.Closed = time.Now()
		}
	}
	s.Lock.Unlock()

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
	if GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.TriggerCapacityPoll()
	}
}

func (s *FileCacheState) IsDiskMaterialized() bool {
	if s == nil {
		return false
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	return s.DiskMaterialized
}

// MaterializeDiskFile creates and truncates the .part file on disk, saves metadata .json,
// and flushes any chunks currently held in memory in LiveBuffer to disk.
func (s *FileCacheState) MaterializeDiskFile() error {
	if s == nil {
		return nil
	}

	var triggerCM *ScratchChunkManager
	defer func() {
		if triggerCM != nil {
			triggerCM.CheckAndTriggerUploads()
		}
	}()

	s.Lock.Lock()
	defer s.Lock.Unlock()

	if (s.DiskMaterialized && s.File != nil) || (s.targetTierLocked() == TierDisk && s.Path != "" && FileExists(s.Path)) {
		return nil
	}

	totalSize := s.TotalSize
	partPath := s.PartPath
	metaPath := s.MetaPath
	url := s.URL
	contentType := s.ContentType
	etag := s.ETag
	lastMod := s.LastModified
	key := s.Key

	var file *os.File
	var err error
	if FileExists(partPath) {
		file, err = os.OpenFile(partPath, os.O_RDWR, 0644)
	} else {
		file, err = os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0644)
	}
	if err != nil {
		log.Printf("[State] Error opening part file %s: %v", partPath, err)
		return err
	}
	if totalSize > 0 {
		_ = file.Truncate(totalSize)
	}

	// Save initial sparse metadata atomically only for persistent TierDisk entries.
	if s.targetTierLocked() == TierDisk && metaPath != "" && (GlobalConfig == nil || GlobalConfig.UsesCacheDir()) {
		meta := FileMeta{
			URL:           url,
			TotalSize:     totalSize,
			ContentType:   contentType,
			ETag:          etag,
			LastModified:  lastMod,
			LastValidated: time.Now().Unix(),
			Completed:     false,
		}
		if metaBytes, merr := json.Marshal(meta); merr == nil {
			_ = writeMetaFileAtomic(metaPath, metaBytes)
		}
	}

	if s.Pipeline != nil {
		if wp, ok := s.Pipeline.(*WritebackPipeline); ok && wp != nil {
			if err := wp.AttachFile(file); err != nil {
				log.Printf("[State] Error attaching file to pipeline for %s: %v", key, err)
			}
		}
	} else if s.LiveBuffer != nil {
		_ = s.LiveBuffer.FlushToFileAndTrack(file, s.DiskRangeSet)
	}

	if s.File != nil && s.File != file {
		_ = s.File.Close()
	}
	s.File = file
	s.DiskMaterialized = true
	triggerCM = s.ChunkManager
	log.Printf("[State] Materialized part file on disk for %s (size: %d bytes)", key, totalSize)
	return nil
}

type CacheRegistry struct {
	Lock             sync.RWMutex
	Files            map[string]*FileCacheState
	Connections      map[string]*ClientConnection
	Sessions         map[string]*ClientSession
	FailedProbes     map[string]time.Time
	probeLRU         []string
	TotalCacheSize   int64
	MaxObservedSpeed int64
	Evicting         bool
	pendingEvict     bool
	lastProbeTime    int64
	stopCh           chan struct{}
	stopOnce         sync.Once
	wg               sync.WaitGroup
}

// Stop terminates background monitor goroutines owned by this CacheRegistry instance.
func (r *CacheRegistry) Stop() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		if r.stopCh != nil {
			close(r.stopCh)
		}
	})
	r.wg.Wait()
}

var GlobalRegistry *CacheRegistry
var GlobalDiskFreeBytes uint64

func (r *CacheRegistry) RecordProbeActivity() {
	if r == nil {
		return
	}
	atomic.StoreInt64(&r.lastProbeTime, time.Now().UnixNano())
}

func (r *CacheRegistry) IsProbeActive() bool {
	if GlobalInFlightProbes.Load() > 0 {
		return true
	}
	if r == nil {
		return false
	}

	// 1. Check if any file in memory is currently in unmaterialized probe stage
	r.Lock.RLock()
	files := make([]*FileCacheState, 0, len(r.Files))
	for _, f := range r.Files {
		files = append(files, f)
	}
	r.Lock.RUnlock()

	for _, f := range files {
		f.Lock.Lock()
		isMat := f.DiskMaterialized
		isPre := f.IsPrehydrating || f.IsYielding
		f.Lock.Unlock()
		if !isMat && !isPre {
			return true
		}
	}

	// 2. Also check if a probe occurred within the last 4 seconds so the UI indicator has time to flash visibly
	last := atomic.LoadInt64(&r.lastProbeTime)
	if last > 0 && time.Since(time.Unix(0, last)) < 4*time.Second {
		return true
	}
	return false
}

func InitRegistry() {
	// Capture config reference on caller goroutine so background monitor never races on GlobalConfig replacement in tests.
	cfg := GlobalConfig
	if cfg != nil {
		InitStorageTopology(cfg)
	}
	if GlobalStorage == nil {
		_ = InitStorage()
	}

	if GlobalRegistry != nil {
		GlobalRegistry.Stop()
	}

	if GlobalS3Index != nil {
		GlobalS3Index.Stop()
		GlobalS3Index = nil
	}

	GlobalRegistry = &CacheRegistry{
		Files:        make(map[string]*FileCacheState),
		Connections:  make(map[string]*ClientConnection),
		Sessions:     make(map[string]*ClientSession),
		FailedProbes: make(map[string]time.Time),
		probeLRU:     make([]string, 0, 128),
		stopCh:       make(chan struct{}),
	}

	GlobalRegistry.LoadExistingCache()

	// Initial population of disk free space
	if cfg != nil {
		diskDir := cfg.GetTmpDir()
		if cfg.UsesCacheDir() {
			diskDir = cfg.GetCacheDir()
		}
		space, _ := GetDiskFreeSpace(diskDir)
		atomic.StoreUint64(&GlobalDiskFreeBytes, space)
	}

	GlobalRegistry.wg.Add(2)
	go GlobalRegistry.startDiskFreeMonitor(cfg)
	go GlobalRegistry.startSpeedMonitor()

	var s3IndexBackend StorageBackend
	if tb, ok := GlobalStorage.(*TieredStorageBackend); ok && tb.S3() != nil {
		s3IndexBackend = tb.S3()
	} else if GlobalStorage != nil && GlobalStorage.Type() == "s3" {
		s3IndexBackend = GlobalStorage
	}

	if s3IndexBackend != nil {
		stateDir := ""
		legacyCacheDir := ""
		if cfg != nil {
			stateDir = cfg.GetStateDir()
			if stateDir == "" {
				stateDir = filepath.Join(cfg.GetTmpDir(), "state")
			}
			legacyCacheDir = cfg.GetCacheDir()
		}
		if stateDir != "" {
			_ = os.MkdirAll(stateDir, 0755)
		}
		indexPath := filepath.Join(stateDir, "s3_index.json")

		// Seamless upgrade migration: move legacy CACHE_DIR/s3_index.json if present and new STATE_DIR file does not exist.
		normLegacy := strings.ToLower(strings.TrimSpace(legacyCacheDir))
		if normLegacy != "" && normLegacy != "none" && normLegacy != "off" && normLegacy != "disabled" {
			legacyPath := filepath.Join(legacyCacheDir, "s3_index.json")
			if legacyPath != indexPath && !FileExists(indexPath) && FileExists(legacyPath) {
				if err := moveFileCrossFS(legacyPath, indexPath); err != nil {
					log.Printf("[S3Index] Warning: Failed to migrate legacy %s to %s: %v", legacyPath, indexPath, err)
				} else {
					log.Printf("[S3Index] Migrated legacy index %s -> %s", legacyPath, indexPath)
				}
			}
		}

		idx := NewS3AccessIndex(indexPath)
		if err := idx.Load(); err != nil {
			log.Printf("[S3Index] Warning: Failed to load %s: %v", indexPath, err)
		}
		GlobalS3Index = idx
		idx.StartBackgroundLoops(s3IndexBackend, GlobalRegistry)
	}
}

// IsKeyInUse reports whether key has an active FileCacheState or open client connection.
func (r *CacheRegistry) IsKeyInUse(key string) bool {
	if r == nil || key == "" {
		return false
	}
	r.Lock.RLock()
	defer r.Lock.RUnlock()
	if _, ok := r.Files[key]; ok {
		return true
	}
	for _, conn := range r.Connections {
		if conn != nil && conn.Key == key {
			return true
		}
	}
	return false
}

func (r *CacheRegistry) RegisterProbeBuffer(key string) {
	if r == nil || key == "" {
		return
	}
	maxProbes := 128
	if GlobalConfig != nil {
		maxProbes = GlobalConfig.GetMaxProbeBuffers()
	}

	r.Lock.Lock()
	for i, k := range r.probeLRU {
		if k == key {
			r.probeLRU = append(r.probeLRU[:i], r.probeLRU[i+1:]...)
			break
		}
	}
	r.probeLRU = append(r.probeLRU, key)

	var toEvict []*FileCacheState
	var evictKeys []string

	for len(r.probeLRU) > maxProbes {
		oldestKey := r.probeLRU[0]
		r.probeLRU = r.probeLRU[1:]
		if state, exists := r.Files[oldestKey]; exists && state != nil {
			delete(r.Files, oldestKey)
			toEvict = append(toEvict, state)
			evictKeys = append(evictKeys, oldestKey)
		}
	}
	r.Lock.Unlock()

	for i, state := range toEvict {
		state.Lock.Lock()
		state.NextURLs = nil
		cancel := state.Cancel
		state.Cancel = nil
		state.WriterActive = false
		if state.LiveBuffer != nil {
			state.LiveBuffer.Clear()
			state.LiveBuffer = nil
		}
		partPath := state.PartPath
		metaPath := state.MetaPath
		state.Lock.Unlock()

		if cancel != nil {
			cancel()
		}
		_ = os.Remove(partPath)
		_ = os.Remove(metaPath)
		log.Printf("[Probe] Evicted oldest probe buffer %s (capacity %d exceeded)", evictKeys[i], maxProbes)
	}

	if len(toEvict) > 0 && GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
}

func (r *CacheRegistry) UnregisterProbeBuffer(key string) {
	if r == nil || key == "" {
		return
	}
	r.Lock.Lock()
	defer r.Lock.Unlock()
	for i, k := range r.probeLRU {
		if k == key {
			r.probeLRU = append(r.probeLRU[:i], r.probeLRU[i+1:]...)
			break
		}
	}
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

func (r *CacheRegistry) startDiskFreeMonitor(cfg *Config) {
	defer r.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	stopCh := r.stopCh
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			if cfg == nil {
				continue
			}
			usesCacheDir := cfg.UsesCacheDir()
			diskDir := cfg.GetTmpDir()
			if usesCacheDir {
				diskDir = cfg.GetCacheDir()
			}
			space, err := GetDiskFreeSpace(diskDir)
			if err == nil {
				atomic.StoreUint64(&GlobalDiskFreeBytes, space)
				if usesCacheDir {
					minFree := EffectiveCacheMinDiskFreeBytes(cfg, GetStorageTopology())
					if minFree > 0 && space < uint64(minFree) {
						r.PreEvict(0)
					}
				}
			}
			r.SweepExpiredProbes()
		}
	}
}

func (r *CacheRegistry) startSpeedMonitor() {
	defer r.wg.Done()
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	stopCh := r.stopCh
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			r.tickSpeedMonitor()
		}
	}
}

func (r *CacheRegistry) tickSpeedMonitor() {
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
			instantSpeed := float64(atomic.SwapInt64(&fileState.BytesDownloadedSec, 0))

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
				fileState.AvgDownloadSpeed = atomic.LoadInt64(&fileState.TotalBytesDownloaded) / elapsed
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
		go GlobalLogBroker.BroadcastStateUpdate()
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
		if conn.CacheSource != "" {
			matchedSession.CacheSource = conn.CacheSource
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
			CacheSource:    conn.CacheSource,
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
	var finalOffset int64 = -1
	var connTotalSize int64
	if r.Connections != nil {
		if cc, ok := r.Connections[connID]; ok {
			finalOffset = atomic.LoadInt64(&cc.Offset)
			connTotalSize = cc.TotalSize
		}
		delete(r.Connections, connID)
	}
	if r.Sessions != nil {
		for _, session := range r.Sessions {
			if _, exists := session.Conns[connID]; exists {
				if finalOffset >= 0 {
					isNearEOF := connTotalSize > 0 && finalOffset >= connTotalSize-10*1024*1024
					sessionIsNearEOF := session.TotalSize > 0 && session.Offset >= session.TotalSize-10*1024*1024
					if len(session.Conns) <= 1 || !isNearEOF || sessionIsNearEOF {
						session.Offset = finalOffset
					}
				}
				delete(session.Conns, connID)
				session.ActiveConns = len(session.Conns)
				session.LastActive = time.Now()
				break
			}
		}
	}
}

func (r *CacheRegistry) UpdateConnectionOffset(connID string, offset int64) {
	r.Lock.RLock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			atomic.StoreInt64(&conn.Offset, offset)
		}
	}
	r.Lock.RUnlock()
}

func (r *CacheRegistry) SetConnectionInCache(connID string, inCache bool) {
	var fs *FileCacheState
	r.Lock.RLock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok && conn.InCache == inCache {
			r.Lock.RUnlock()
			return
		} else if ok && inCache && conn.CacheSource == "" {
			fs = r.Files[conn.Key]
		}
	}
	r.Lock.RUnlock()

	resolvedSource := ""
	if inCache {
		if fs != nil {
			resolvedSource = string(fs.GetTargetTier())
		} else if GlobalStorage != nil && GlobalStorage.Type() != "" && GlobalStorage.Type() != "hybrid" {
			resolvedSource = GlobalStorage.Type()
		} else {
			resolvedSource = string(TierDisk)
		}
	}

	r.Lock.Lock()
	defer r.Lock.Unlock()
	if r.Connections != nil {
		if conn, ok := r.Connections[connID]; ok {
			conn.InCache = inCache
			if inCache && conn.CacheSource == "" {
				conn.CacheSource = resolvedSource
			}
			if r.Sessions != nil {
				for _, session := range r.Sessions {
					if _, exists := session.Conns[connID]; exists {
						if !inCache {
							session.InCache = false
						}
						if conn.CacheSource != "" {
							session.CacheSource = conn.CacheSource
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
							ccOffset := atomic.LoadInt64(&cc.Offset)
							isNearEOF := cc.TotalSize > 0 && ccOffset >= cc.TotalSize-10*1024*1024
							isProbe := isNearEOF && (ccOffset-cc.StartOffset) <= 2*1024*1024
							var initialMaxOffset int64
							if !isProbe {
								initialMaxOffset = ccOffset
							}

							newSession := &ClientSession{
								ID:             cc.ID,
								Key:            cc.Key,
								URL:            cc.URL,
								RemoteAddr:     cc.RemoteAddr,
								UserAgent:      cc.UserAgent,
								Username:       cc.Username,
								Offset:         ccOffset,
								MaxOffset:      initialMaxOffset,
								TotalSize:      cc.TotalSize,
								StartTime:      cc.StartTime,
								LastActive:     now,
								InCache:        cc.InCache,
								CacheSource:    cc.CacheSource,
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
				ccOffset := atomic.LoadInt64(&cc.Offset)
				isNearEOF := cc.TotalSize > 0 && ccOffset >= cc.TotalSize-10*1024*1024
				wasNotPreceded := session.MaxOffset < cc.TotalSize-15*1024*1024
				readLimitNotExceeded := (ccOffset - cc.StartOffset) <= 2*1024*1024
				isProbe := isNearEOF && wasNotPreceded && readLimitNotExceeded

				if !isProbe {
					nonProbeCount++
					if oldestNonProbeTime.IsZero() || info.Opened.Before(oldestNonProbeTime) {
						oldestNonProbeTime = info.Opened
						oldestNonProbeOffset = ccOffset
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
// and delete any dangling scratch/staging files from previous runs.
func (r *CacheRegistry) LoadExistingCache() {
	r.Lock.Lock()
	defer r.Lock.Unlock()

	usesCacheDir := GlobalConfig == nil || GlobalConfig.UsesCacheDir()
	cacheDir := ""
	tmpDir := ""
	stateDir := ""
	if GlobalConfig != nil {
		cacheDir = GlobalConfig.GetCacheDir()
		tmpDir = GlobalConfig.GetTmpDir()
		stateDir = GlobalConfig.GetStateDir()
	}

	if tmpDir != "" {
		_ = os.MkdirAll(tmpDir, 0755)
	}
	if stateDir != "" {
		_ = os.MkdirAll(stateDir, 0755)
	}
	if usesCacheDir && cacheDir != "" {
		_ = os.MkdirAll(cacheDir, 0755)
	}

	var totalSize int64

	// 1. Scan completed cache files and clean up orphaned metadata/staging files (only when CACHE_DIR is enabled)
	if usesCacheDir && cacheDir != "" {
		_ = filepath.WalkDir(cacheDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() {
				base := filepath.Base(path)
				if base == "s3_index.json" {
					return nil
				}
				if strings.HasPrefix(base, "s3_index.json.tmp.") {
					_ = os.Remove(path)
					return nil
				}
				ext := filepath.Ext(path)
				if ext == ".staging" || ext == ".part" {
					_ = os.Remove(path)
					return nil
				}
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
	}

	// 2. Clean up temporary directory on boot, preserving subdirectories such as STATE_DIR (e.g. TMP_DIR/state).
	if tmpDir != "" {
		files, err := os.ReadDir(tmpDir)
		if err == nil {
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				_ = os.Remove(filepath.Join(tmpDir, f.Name()))
			}
		}
	}

	atomic.StoreInt64(&r.TotalCacheSize, totalSize)
	log.Printf("[Registry] Scan complete. Initial Cache Size: %.2f GB (UsesCacheDir=%v)",
		float64(totalSize)/(1024*1024*1024), usesCacheDir)
}

// CanAdmitToDiskL1 decides whether an incoming item of totalSize bytes can be admitted to the persistent
// CACHE_DIR (TierDisk) in disk mode without violating CACHE_MAX_ITEM_PCT, evicting active/pinned files,
// or breaching CACHE_MAX_BYTES / EffectiveCacheMinDiskFreeBytes. Otherwise callers route to TierTmp.
func (r *CacheRegistry) CanAdmitToDiskL1(totalSize int64) (bool, string) {
	return r.canAdmitToL1(totalSize, true)
}

// CanAdmitToHybridL1 decides whether an incoming item can be admitted to CACHE_DIR in hybrid mode
// using only unallocated L1 headroom (without forcing eviction of warm L1 items or active viewers).
// Items not admitted bypass cleanly to TierS3.
func (r *CacheRegistry) CanAdmitToHybridL1(totalSize int64) (bool, string) {
	return r.canAdmitToL1(totalSize, false)
}

func (r *CacheRegistry) canAdmitToL1(totalSize int64, allowEviction bool) (bool, string) {
	if GlobalConfig == nil {
		return true, ""
	}
	if !GlobalConfig.UsesCacheDir() {
		return false, "cache_dir_disabled"
	}
	if totalSize <= 0 {
		if !allowEviction {
			return false, "unknown_size_bypass"
		}
		return true, ""
	}

	maxBytes := GlobalConfig.GetCacheMaxBytes()
	maxItemPct := GlobalConfig.GetCacheMaxItemPct()
	if maxBytes > 0 && maxItemPct > 0 {
		maxItemBytes := int64(math.Round(float64(maxBytes) * float64(maxItemPct) / 100.0))
		if totalSize > maxItemBytes {
			return false, fmt.Sprintf("item %d bytes exceeds CACHE_MAX_ITEM_PCT limit (%d%% of %d = %d bytes)",
				totalSize, maxItemPct, maxBytes, maxItemBytes)
		}
	}

	activeKeys := make(map[string]bool)
	var activeFiles []*FileCacheState
	if r != nil {
		r.Lock.RLock()
		activeFiles = make([]*FileCacheState, 0, len(r.Files))
		for k, f := range r.Files {
			activeKeys[k] = true
			if f != nil {
				activeFiles = append(activeFiles, f)
			}
		}
		for _, conn := range r.Connections {
			if conn != nil && conn.Key != "" {
				activeKeys[conn.Key] = true
			}
		}
		r.Lock.RUnlock()
	}

	cacheDir := GlobalConfig.GetCacheDir()
	var pinnedL1Bytes int64
	var inactiveEvictableBytes int64
	pinnedHandled := make(map[string]bool)
	stagedBytes := make(map[string]int64)

	if entries, err := os.ReadDir(cacheDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if strings.HasPrefix(name, "s3_index.json") || filepath.Ext(name) == ".json" || filepath.Ext(name) == ".part" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if filepath.Ext(name) == ".staging" {
				baseKey := strings.TrimSuffix(name, ".staging")
				stagedBytes[baseKey] = info.Size()
				continue
			}
			if activeKeys[name] {
				pinnedL1Bytes += info.Size()
				pinnedHandled[name] = true
			} else {
				inactiveEvictableBytes += info.Size()
			}
		}
	}

	var unwrittenActiveL1Bytes int64
	for _, f := range activeFiles {
		f.Lock.Lock()
		key := f.Key
		isMat := f.DiskMaterialized
		isYield := f.IsYielding
		totalSz := f.TotalSize
		tier := f.targetTierLocked()
		cm := f.ChunkManager
		f.Lock.Unlock()

		if isYield || tier != TierDisk || totalSz <= 0 || pinnedHandled[key] {
			continue
		}
		pinnedL1Bytes += totalSz
		if cm != nil && cm.TargetName() == "nas" {
			rem := totalSz - stagedBytes[key]
			if rem > 0 {
				unwrittenActiveL1Bytes += rem
			}
		} else if isMat {
			downloaded := atomic.LoadInt64(&f.TotalBytesDownloaded)
			if rem := totalSz - downloaded; rem > 0 {
				unwrittenActiveL1Bytes += rem
			}
		} else {
			unwrittenActiveL1Bytes += totalSz
		}
	}

	if maxBytes > 0 {
		committedL1 := pinnedL1Bytes
		if !allowEviction {
			committedL1 += inactiveEvictableBytes
		}
		if committedL1+totalSize > maxBytes {
			return false, fmt.Sprintf("L1 occupancy (%d) + item (%d) exceeds CACHE_MAX_BYTES (%d) (allowEviction=%v, inactive=%d)",
				committedL1, totalSize, maxBytes, allowEviction, inactiveEvictableBytes)
		}
	}

	minDiskFree := EffectiveCacheMinDiskFreeBytes(GlobalConfig, GetStorageTopology())
	if freeSpace, spaceErr := GetDiskFreeSpace(cacheDir); spaceErr == nil {
		var required uint64
		if totalSize > 0 {
			required += uint64(totalSize)
		}
		if unwrittenActiveL1Bytes > 0 {
			required += uint64(unwrittenActiveL1Bytes)
		}
		if minDiskFree > 0 {
			required += uint64(minDiskFree)
		}
		maxAvailable := freeSpace
		if allowEviction && inactiveEvictableBytes > 0 {
			if freeSpace > math.MaxUint64-uint64(inactiveEvictableBytes) {
				maxAvailable = math.MaxUint64
			} else {
				maxAvailable += uint64(inactiveEvictableBytes)
			}
		}
		if maxAvailable < required {
			return false, fmt.Sprintf("insufficient L1 disk space (available %d < required %d, allowEviction=%v)",
				maxAvailable, required, allowEviction)
		}
	}

	return true, ""
}

func (r *CacheRegistry) UpdateSize(delta int64) {
	atomic.AddInt64(&r.TotalCacheSize, delta)
}

func (r *CacheRegistry) GetTotalCacheSize() int64 {
	return atomic.LoadInt64(&r.TotalCacheSize)
}

func (r *CacheRegistry) PreEvict(incomingSizeBytes int64) {
	if GlobalConfig != nil && !GlobalConfig.UsesCacheDir() {
		return
	}
	r.Lock.Lock()
	if r.Evicting {
		r.pendingEvict = true
		r.Lock.Unlock()
		return
	}

	r.Evicting = true
	r.pendingEvict = false
	files := make([]*FileCacheState, 0, len(r.Files))
	activeKeys := make(map[string]bool, len(r.Files)+len(r.Connections))
	for k, f := range r.Files {
		activeKeys[k] = true
		if f != nil {
			files = append(files, f)
		}
	}
	for _, conn := range r.Connections {
		if conn != nil && conn.Key != "" {
			activeKeys[conn.Key] = true
		}
	}
	r.Lock.Unlock()

	var unwrittenActiveBytes int64
	for _, f := range files {
		f.Lock.Lock()
		isMat := f.DiskMaterialized
		isYield := f.IsYielding
		totalSz := f.TotalSize
		tier := f.targetTierLocked()
		f.Lock.Unlock()
		if isMat && !isYield && tier == TierDisk && totalSz > 0 {
			downloaded := atomic.LoadInt64(&f.TotalBytesDownloaded)
			if rem := totalSz - downloaded; rem > 0 {
				unwrittenActiveBytes += rem
			}
		}
	}

	totalPending := incomingSizeBytes + unwrittenActiveBytes
	currentSize := atomic.LoadInt64(&r.TotalCacheSize)
	cacheDir := GlobalConfig.GetCacheDir()

	// Calculate how much space we need to free based on cache limit if incoming + unwritten active bytes are added
	var cacheBytesToFree int64
	maxBytes := GlobalConfig.GetCacheMaxBytes()
	minDiskFree := EffectiveCacheMinDiskFreeBytes(GlobalConfig, GetStorageTopology())
	if maxBytes > 0 && (currentSize+totalPending) > maxBytes {
		cacheBytesToFree = (currentSize + totalPending) - maxBytes
	}

	// Calculate how much space we need to free based on disk space limit
	var diskBytesToFree int64
	freeSpace, spaceErr := GetDiskFreeSpace(cacheDir)
	if spaceErr == nil && minDiskFree > 0 {
		requiredFree := uint64(totalPending) + uint64(minDiskFree)
		if freeSpace < requiredFree {
			diskBytesToFree = int64(requiredFree - freeSpace)
		}
	}

	bytesToFree := cacheBytesToFree
	if diskBytesToFree > bytesToFree {
		bytesToFree = diskBytesToFree
	}

	if bytesToFree <= 0 {
		r.Lock.Lock()
		reRun := r.pendingEvict
		r.pendingEvict = false
		r.Evicting = false
		r.Lock.Unlock()
		if reRun {
			r.PreEvict(0)
		}
		return
	}

	// Fast pre-flight check outside GlobalRegistry.Lock: count total size of available inactive files
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		r.Lock.Lock()
		r.pendingEvict = false
		r.Evicting = false
		r.Lock.Unlock()
		return
	}

	var availableFreeable int64
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "s3_index.json") || filepath.Ext(entry.Name()) == ".json" || filepath.Ext(entry.Name()) == ".part" || filepath.Ext(entry.Name()) == ".staging" {
			continue
		}
		if !activeKeys[entry.Name()] {
			if info, err := entry.Info(); err == nil {
				availableFreeable += info.Size()
			}
		}
	}

	if availableFreeable < bytesToFree {
		log.Printf("[Evictor] WARNING: Cache pre-flight check required %.2f MB, but only %.2f MB of inactive files are available.",
			float64(bytesToFree)/(1024*1024), float64(availableFreeable)/(1024*1024))
	}

	log.Printf("[Evictor] Fast pre-flight check passed. Spawning background evictor for incoming file (%.2f MB, active unwritten: %.2f MB). Cache size: %.2f GB (limit: %.2f GB), Free Disk Space: %.2f GB (min: %.2f GB). Need to free %.2f MB.",
		float64(incomingSizeBytes)/(1024*1024),
		float64(unwrittenActiveBytes)/(1024*1024),
		float64(currentSize)/(1024*1024*1024), float64(maxBytes)/(1024*1024*1024),
		float64(freeSpace)/(1024*1024*1024), float64(minDiskFree)/(1024*1024*1024),
		float64(bytesToFree)/(1024*1024))

	// Launch file deletions asynchronously in a background thread to prevent blocking HTTP handler/stream start
	go func() {
		r.evictInactiveFiles(bytesToFree)
		r.Lock.Lock()
		reRun := r.pendingEvict
		r.pendingEvict = false
		r.Evicting = false
		r.Lock.Unlock()
		if reRun {
			r.PreEvict(0)
		}
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
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "s3_index.json") || filepath.Ext(entry.Name()) == ".json" || filepath.Ext(entry.Name()) == ".part" || filepath.Ext(entry.Name()) == ".staging" {
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

		// Prevent evicting files that are actively being downloaded or streamed (including disk cache HITs)
		baseKey := strings.TrimSuffix(file.Name, ".part")
		if r.IsKeyInUse(baseKey) {
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
