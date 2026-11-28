package main

import (
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// StorageTopology captures startup-detected mount properties, hole-punch support,
// and clamped scratch/cache space limits across TMP_DIR, CACHE_DIR, and STATE_DIR.
type StorageTopology struct {
	TmpDir                string
	CacheDir              string
	StateDir              string
	UsesCacheDir          bool
	SameFilesystem        bool
	CrossFSStaging        bool
	TmpFSType             string
	CacheFSType           string
	TmpMountTotalBytes    uint64
	TmpPunchHoleSupported bool
	SequentialNASStage    bool
	EffectiveTmpMinFree   int64
	EffectiveCacheMinFree int64
}

var (
	globalTopologyMu sync.RWMutex
	globalTopology   *StorageTopology

	diskFreeSpaceMu       sync.RWMutex
	diskFreeSpaceProvider = GetDiskFreeSpace
)

func queryDiskFreeSpace(path string) (uint64, error) {
	diskFreeSpaceMu.RLock()
	fn := diskFreeSpaceProvider
	diskFreeSpaceMu.RUnlock()
	if fn == nil {
		return GetDiskFreeSpace(path)
	}
	return fn(path)
}

// computeEffectiveL1CapBytes returns the active L1 capacity ceiling as min(CACHE_MAX_BYTES, diskPool),
// where diskPool = max(0, onDiskL1Bytes + freeSpace - minDiskFree). When CACHE_MAX_BYTES == 0 and
// filesystem free space is unavailable, it returns (0, false).
func computeEffectiveL1CapBytes(configuredMaxBytes, minDiskFree, onDiskL1Bytes int64, freeSpace uint64, hasFreeSpace bool) (int64, bool) {
	if !hasFreeSpace {
		if configuredMaxBytes > 0 {
			return configuredMaxBytes, true
		}
		return 0, false
	}

	if onDiskL1Bytes < 0 {
		onDiskL1Bytes = 0
	}
	if minDiskFree < 0 {
		minDiskFree = 0
	}

	var diskPool int64
	if freeSpace >= uint64(minDiskFree) {
		avail := freeSpace - uint64(minDiskFree)
		if avail > uint64(math.MaxInt64-onDiskL1Bytes) {
			diskPool = math.MaxInt64
		} else {
			diskPool = onDiskL1Bytes + int64(avail)
		}
	} else {
		deficit := uint64(minDiskFree) - freeSpace
		if deficit >= uint64(onDiskL1Bytes) {
			diskPool = 0
		} else {
			diskPool = onDiskL1Bytes - int64(deficit)
		}
	}

	if configuredMaxBytes > 0 && configuredMaxBytes < diskPool {
		return configuredMaxBytes, true
	}
	return diskPool, true
}

// GetStorageTopology returns the most recently initialized StorageTopology (or nil before startup).
func GetStorageTopology() *StorageTopology {
	globalTopologyMu.RLock()
	defer globalTopologyMu.RUnlock()
	if globalTopology == nil {
		return nil
	}
	cp := *globalTopology
	return &cp
}

// SetStorageTopology replaces the global StorageTopology snapshot.
func SetStorageTopology(t *StorageTopology) {
	globalTopologyMu.Lock()
	defer globalTopologyMu.Unlock()
	if t == nil {
		globalTopology = nil
		return
	}
	cp := *t
	globalTopology = &cp
}

func unescapeMountInfoPath(p string) string {
	r := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return filepath.Clean(r.Replace(p))
}

func parseMountInfoFSType(data, targetPath string) string {
	cleanTarget := filepath.Clean(targetPath)
	bestLen := -1
	bestFS := ""

	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		sepIdx := -1
		for i, f := range fields {
			if f == "-" {
				sepIdx = i
				break
			}
		}
		if sepIdx < 5 || sepIdx+1 >= len(fields) {
			continue
		}

		mountPoint := unescapeMountInfoPath(fields[4])
		matches := cleanTarget == mountPoint ||
			mountPoint == "/" ||
			strings.HasPrefix(cleanTarget, mountPoint+"/")
		if !matches {
			continue
		}
		if len(mountPoint) > bestLen {
			bestLen = len(mountPoint)
			bestFS = strings.ToLower(strings.TrimSpace(fields[sepIdx+1]))
		}
	}

	return bestFS
}

func resolveSequentialNASStage(mode, cacheFSType string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "true":
		return true
	case "false":
		return false
	default:
		fsLower := strings.ToLower(strings.TrimSpace(cacheFSType))
		return fsLower == "cifs" ||
			fsLower == "smb" ||
			fsLower == "smb2" ||
			fsLower == "smb3" ||
			strings.HasPrefix(fsLower, "fuse")
	}
}

func computeEffectiveLimits(cfgTmpMinFree, cfgTmpFileMax, cfgCacheMinFree int64, tmpMountTotal uint64, usesCacheDir, sameFS bool) (effectiveTmpMinFree, effectiveCacheMinFree int64, tmpClamped, cacheRaised bool) {
	effectiveTmpMinFree = cfgTmpMinFree
	if tmpMountTotal > 0 && cfgTmpMinFree > 0 {
		quarter := int64(tmpMountTotal / 4)
		if quarter > 0 && cfgTmpMinFree > quarter {
			effectiveTmpMinFree = quarter
			tmpClamped = true
		}
	}

	effectiveCacheMinFree = cfgCacheMinFree
	if usesCacheDir && sameFS && (cfgCacheMinFree > 0 || effectiveTmpMinFree > 0) {
		floor := effectiveTmpMinFree + cfgTmpFileMax
		if floor > effectiveCacheMinFree {
			effectiveCacheMinFree = floor
			cacheRaised = true
		}
	}
	return effectiveTmpMinFree, effectiveCacheMinFree, tmpClamped, cacheRaised
}

func probePunchHoleSupport(tmpDir string) bool {
	if tmpDir == "" {
		return false
	}
	f, err := os.CreateTemp(tmpDir, ".sc_punch_probe_*")
	if err != nil {
		return false
	}
	probePath := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(probePath)
	}()

	const probeSize = 64 * 1024
	if err := f.Truncate(probeSize); err != nil {
		return false
	}
	if _, err := f.WriteAt([]byte("streamcache-probe"), 0); err != nil {
		return false
	}
	return PunchHole(f.Fd(), 0, probeSize) == nil
}

// InitStorageTopology initializes required directories according to EffectiveStorageBackend(),
// detects mount types / cross-filesystem boundaries / hole-punch support, and computes effective limits.
func InitStorageTopology(cfg *Config) *StorageTopology {
	if cfg == nil {
		SetStorageTopology(nil)
		return nil
	}

	tmpDir := cfg.GetTmpDir()
	cacheDir := cfg.GetCacheDir()
	stateDir := cfg.GetStateDir()
	usesCacheDir := cfg.UsesCacheDir()

	if tmpDir != "" {
		if err := os.MkdirAll(tmpDir, 0755); err != nil {
			log.Printf("[Topology] WARNING: Failed to create TMP_DIR %s: %v", tmpDir, err)
		}
	}
	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0755); err != nil {
			log.Printf("[Topology] WARNING: Failed to create STATE_DIR %s: %v", stateDir, err)
		}
	}
	if usesCacheDir && cacheDir != "" {
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Printf("[Topology] WARNING: Failed to create CACHE_DIR %s: %v", cacheDir, err)
		}
	}

	topo := &StorageTopology{
		TmpDir:       tmpDir,
		CacheDir:     cacheDir,
		StateDir:     stateDir,
		UsesCacheDir: usesCacheDir,
		TmpFSType:    detectFSType(tmpDir),
		CacheFSType:  "none",
	}

	if _, totalBytes, err := GetDiskCapacity(tmpDir); err == nil {
		topo.TmpMountTotalBytes = totalBytes
	}
	topo.TmpPunchHoleSupported = probePunchHoleSupport(tmpDir)
	if !topo.TmpPunchHoleSupported {
		log.Printf("[Topology] WARNING: TMP_DIR (%s, fstype=%s) does not support fallocate(FALLOC_FL_PUNCH_HOLE); scratch deallocation will be a no-op", tmpDir, topo.TmpFSType)
	}

	if usesCacheDir && cacheDir != "" {
		topo.CacheFSType = detectFSType(cacheDir)
		tmpDev, tmpOK := getDeviceID(tmpDir)
		cacheDev, cacheOK := getDeviceID(cacheDir)
		if tmpOK && cacheOK {
			topo.SameFilesystem = tmpDev == cacheDev
			topo.CrossFSStaging = !topo.SameFilesystem
		} else {
			topo.SameFilesystem = true
			topo.CrossFSStaging = false
		}
	}

	topo.SequentialNASStage = resolveSequentialNASStage(cfg.GetNASSequentialStage(), topo.CacheFSType)

	var tmpClamped, cacheRaised bool
	topo.EffectiveTmpMinFree, topo.EffectiveCacheMinFree, tmpClamped, cacheRaised = computeEffectiveLimits(
		cfg.GetTmpMinDiskFreeBytes(),
		cfg.GetTmpFileMaxBytes(),
		cfg.GetCacheMinDiskFreeBytes(),
		topo.TmpMountTotalBytes,
		usesCacheDir,
		topo.SameFilesystem,
	)

	if tmpClamped {
		log.Printf("[Topology] WARNING: TMP_MIN_DISK_FREE_BYTES (%d MB) exceeded 25%% of TMP_DIR mount (%d MB, fstype=%s); clamped to %d MB",
			cfg.GetTmpMinDiskFreeBytes()/(1024*1024),
			topo.TmpMountTotalBytes/(1024*1024),
			topo.TmpFSType,
			topo.EffectiveTmpMinFree/(1024*1024),
		)
	}
	if cacheRaised {
		log.Printf("[Topology] Shared filesystem detected between TMP_DIR (%s) and CACHE_DIR (%s); raising effective CACHE_MIN_DISK_FREE_BYTES from %d MB to %d MB to preserve scratch headroom",
			tmpDir,
			cacheDir,
			cfg.GetCacheMinDiskFreeBytes()/(1024*1024),
			topo.EffectiveCacheMinFree/(1024*1024),
		)
	}

	log.Printf("[Topology] Initialized: EffectiveBackend=%s, UsesCacheDir=%v, TmpDir=%s (fstype=%s, punchHole=%v), CacheDir=%s (fstype=%s), StateDir=%s, SameFS=%v, CrossFSStaging=%v, NASSequentialStage=%v, EffectiveTmpMinFree=%dMB, EffectiveCacheMinFree=%dMB",
		cfg.EffectiveStorageBackend(),
		topo.UsesCacheDir,
		topo.TmpDir,
		topo.TmpFSType,
		topo.TmpPunchHoleSupported,
		topo.CacheDir,
		topo.CacheFSType,
		topo.StateDir,
		topo.SameFilesystem,
		topo.CrossFSStaging,
		topo.SequentialNASStage,
		topo.EffectiveTmpMinFree/(1024*1024),
		topo.EffectiveCacheMinFree/(1024*1024),
	)

	SetStorageTopology(topo)
	return topo
}

// EffectiveTmpMinDiskFreeBytes returns the active TMP_DIR free-space floor after applying small-mount clamping.
func EffectiveTmpMinDiskFreeBytes(cfg *Config, topo *StorageTopology) int64 {
	if cfg == nil {
		if topo != nil {
			return topo.EffectiveTmpMinFree
		}
		return 1 * 1024 * 1024 * 1024
	}
	cfgTmpMin := cfg.GetTmpMinDiskFreeBytes()
	if topo != nil && topo.TmpDir == cfg.GetTmpDir() && topo.TmpMountTotalBytes > 0 {
		effTmp, _, _, _ := computeEffectiveLimits(
			cfgTmpMin,
			cfg.GetTmpFileMaxBytes(),
			cfg.GetCacheMinDiskFreeBytes(),
			topo.TmpMountTotalBytes,
			topo.UsesCacheDir,
			topo.SameFilesystem,
		)
		return effTmp
	}
	return cfgTmpMin
}

// EffectiveCacheMinDiskFreeBytes returns the active CACHE_DIR free-space floor, accounting for shared-volume scratch headroom.
func EffectiveCacheMinDiskFreeBytes(cfg *Config, topo *StorageTopology) int64 {
	if cfg == nil {
		if topo != nil {
			return topo.EffectiveCacheMinFree
		}
		return 10 * 1024 * 1024 * 1024
	}
	cfgCacheMin := cfg.GetCacheMinDiskFreeBytes()
	if topo != nil && topo.UsesCacheDir && topo.TmpDir == cfg.GetTmpDir() && topo.CacheDir == cfg.GetCacheDir() {
		_, effCache, _, _ := computeEffectiveLimits(
			cfg.GetTmpMinDiskFreeBytes(),
			cfg.GetTmpFileMaxBytes(),
			cfgCacheMin,
			topo.TmpMountTotalBytes,
			topo.UsesCacheDir,
			topo.SameFilesystem,
		)
		return effCache
	}
	return cfgCacheMin
}
