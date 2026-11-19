package main

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	mu                            sync.RWMutex
	Port                          string
	CacheDir                      string
	TmpDir                        string
	StateDir                      string
	CacheMaxBytes                 int64
	CacheMaxItemPct               int
	CacheMinDiskFreeBytes         int64
	TmpFileMaxBytes               int64
	TmpMinDiskFreeBytes           int64
	CacheRevalidateInterval       time.Duration
	AllowedHosts                  []string
	SchedulerMinTime              time.Duration
	UpstreamReadTimeout           time.Duration
	MaxUpstreamWorkers            int
	ClientHydrationWait           time.Duration
	ProbeFailureCooldown          time.Duration
	ProbeTransientCooldown        time.Duration
	UpstreamUserAgent             string
	UpstreamUserAgentMode         string
	MaxUpstreamRetries            int
	StarvationStallTimeout        time.Duration
	StallTeardownCooldown         time.Duration
	LogVerbose                    bool
	WritebackBufferSize           int64
	StorageBackend                string
	NASSequentialStage            string
	HybridDemoteHighWatermark     int
	HybridDemoteLowWatermark      int
	hybridLowWatermarkSet         bool
	HybridEagerS3Backup           bool
	S3Config                      S3Config
	CacheMaxIdleAge               time.Duration
	CacheEvictInterval            time.Duration
	DispatcharrURL                string
	PrehydrateCapacityReservation int
	DispatcharrPollInterval       time.Duration
	PrehydrateYieldBackoff        time.Duration
	EvictYieldedParts             bool
	EnableEpisodePrehydration     bool
	MinReadThresholdBytes         int64
	MaxProbeBuffers               int
}

func (c *Config) Lock() {
	if c != nil {
		c.mu.Lock()
	}
}

func (c *Config) Unlock() {
	if c != nil {
		c.mu.Unlock()
	}
}

func (c *Config) RLock() {
	if c != nil {
		c.mu.RLock()
	}
}

func (c *Config) RUnlock() {
	if c != nil {
		c.mu.RUnlock()
	}
}

func (c *Config) GetPort() string {
	if c == nil {
		return ":8080"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.Port
}

func (c *Config) GetAllowedHosts() []string {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.AllowedHosts == nil {
		return nil
	}
	hosts := make([]string, len(c.AllowedHosts))
	copy(hosts, c.AllowedHosts)
	return hosts
}

func (c *Config) GetCacheDir() string {
	if c == nil {
		return "./cache/files"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CacheDir
}

func (c *Config) GetTmpDir() string {
	if c == nil {
		return "./cache/tmp"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TmpDir
}

func (c *Config) GetStateDir() string {
	if c == nil {
		return filepath.Join("./cache/tmp", "state")
	}
	c.mu.RLock()
	stateDir := strings.TrimSpace(c.StateDir)
	tmpDir := c.TmpDir
	c.mu.RUnlock()
	if stateDir != "" {
		return stateDir
	}
	if strings.TrimSpace(tmpDir) == "" {
		tmpDir = "./cache/tmp"
	}
	return filepath.Join(tmpDir, "state")
}

func (c *Config) SetStateDir(val string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.StateDir = val
}

func normalizeCacheMaxItemPct(val int) int {
	if val < 0 {
		return 50
	}
	if val > 100 {
		return 100
	}
	return val
}

func (c *Config) GetCacheMaxItemPct() int {
	if c == nil {
		return 50
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeCacheMaxItemPct(c.CacheMaxItemPct)
}

func (c *Config) SetCacheMaxItemPct(val int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CacheMaxItemPct = normalizeCacheMaxItemPct(val)
}

func (c *Config) GetNASSequentialStage() string {
	if c == nil {
		return "auto"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	mode := strings.ToLower(strings.TrimSpace(c.NASSequentialStage))
	switch mode {
	case "true", "false":
		return mode
	default:
		return "auto"
	}
}

func (c *Config) SetNASSequentialStage(val string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.NASSequentialStage = val
}

func normalizeHighWatermark(val int) int {
	if val <= 0 {
		return 80
	}
	if val > 100 {
		return 100
	}
	return val
}

func (c *Config) GetHybridDemoteHighWatermark() int {
	if c == nil {
		return 80
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeHighWatermark(c.HybridDemoteHighWatermark)
}

func (c *Config) SetHybridDemoteHighWatermark(val int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.HybridDemoteHighWatermark = normalizeHighWatermark(val)
}

func (c *Config) GetHybridDemoteLowWatermark() int {
	if c == nil {
		return 70
	}
	c.mu.RLock()
	high := normalizeHighWatermark(c.HybridDemoteHighWatermark)
	low := c.HybridDemoteLowWatermark
	lowSet := c.hybridLowWatermarkSet
	c.mu.RUnlock()

	if !lowSet && low <= 0 {
		low = 70
	} else if low < 0 {
		low = 70
	}
	if low > high {
		low = high
	}
	return low
}

func (c *Config) SetHybridDemoteLowWatermark(val int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if val < 0 {
		val = 70
	} else if val > 100 {
		val = 100
	}
	c.HybridDemoteLowWatermark = val
	c.hybridLowWatermarkSet = true
}

func (c *Config) GetHybridEagerS3Backup() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.HybridEagerS3Backup
}

func (c *Config) SetHybridEagerS3Backup(val bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.HybridEagerS3Backup = val
}

func (c *Config) GetMaxUpstreamWorkers() int {
	if c == nil {
		return 2
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MaxUpstreamWorkers <= 0 {
		return 2
	}
	return c.MaxUpstreamWorkers
}

func (c *Config) GetSchedulerMinTime() time.Duration {
	if c == nil {
		return 2 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.SchedulerMinTime
}

func (c *Config) GetClientHydrationWait() time.Duration {
	if c == nil {
		return 2 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ClientHydrationWait
}

func (c *Config) GetUpstreamReadTimeout() time.Duration {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.UpstreamReadTimeout
}

func (c *Config) GetMaxUpstreamRetries() int {
	if c == nil {
		return 0
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MaxUpstreamRetries
}

func (c *Config) GetStarvationStallTimeout() time.Duration {
	if c == nil {
		return 35 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.StarvationStallTimeout <= 0 {
		return 35 * time.Second
	}
	return c.StarvationStallTimeout
}

func (c *Config) GetStallTeardownCooldown() time.Duration {
	if c == nil {
		return 5 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.StallTeardownCooldown <= 0 {
		return 5 * time.Second
	}
	return c.StallTeardownCooldown
}

func (c *Config) GetCacheMaxBytes() int64 {
	if c == nil {
		return 50 * 1024 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CacheMaxBytes
}

func (c *Config) GetCacheMinDiskFreeBytes() int64 {
	if c == nil {
		return 10 * 1024 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CacheMinDiskFreeBytes
}

func (c *Config) SetCacheMinDiskFreeBytes(val int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CacheMinDiskFreeBytes = val
}

func (c *Config) GetTmpFileMaxBytes() int64 {
	if c == nil {
		return 512 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.TmpFileMaxBytes <= 0 {
		return 512 * 1024 * 1024
	}
	return c.TmpFileMaxBytes
}

func (c *Config) SetTmpFileMaxBytes(val int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.TmpFileMaxBytes = val
}

func (c *Config) GetTmpMinDiskFreeBytes() int64 {
	if c == nil {
		return 1 * 1024 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.TmpMinDiskFreeBytes
}

func (c *Config) SetTmpMinDiskFreeBytes(val int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.TmpMinDiskFreeBytes = val
}

func (c *Config) GetCacheRevalidateInterval() time.Duration {
	if c == nil {
		return 1 * time.Hour
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CacheRevalidateInterval
}

func (c *Config) GetProbeFailureCooldown() time.Duration {
	if c == nil {
		return 10 * time.Minute
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ProbeFailureCooldown
}

func (c *Config) GetProbeTransientCooldown() time.Duration {
	if c == nil {
		return 30 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ProbeTransientCooldown
}

func (c *Config) GetUpstreamUserAgent() string {
	if c == nil {
		return "VLC/3.0.20 LibVLC/3.0.20"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.UpstreamUserAgent == "" {
		return "VLC/3.0.20 LibVLC/3.0.20"
	}
	return c.UpstreamUserAgent
}

func (c *Config) GetUpstreamUserAgentMode() string {
	if c == nil {
		return "smart"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	mode := strings.ToLower(strings.TrimSpace(c.UpstreamUserAgentMode))
	if mode == "" {
		return "smart"
	}
	return mode
}

func (c *Config) GetLogVerbose() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.LogVerbose
}

func (c *Config) SetLogVerbose(val bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.LogVerbose = val
}

func (c *Config) GetWritebackBufferSize() int64 {
	if c == nil {
		return 64 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.WritebackBufferSize <= 0 {
		return 64 * 1024 * 1024
	}
	return c.WritebackBufferSize
}

func (c *Config) SetWritebackBufferSize(val int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.WritebackBufferSize = val
}

type S3Config struct {
	Endpoint             string
	Bucket               string
	Region               string
	AccessKey            string
	SecretKey            string
	UsePathStyle         bool
	DisableSSL           bool
	ChunkSize            int64
	UploadConcurrency    int
	RecomposeConcurrency int
}

func (c *Config) GetStorageBackend() string {
	if c == nil {
		return "disk"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	mode := strings.ToLower(strings.TrimSpace(c.StorageBackend))
	if mode == "" {
		return "disk"
	}
	return mode
}

func (c *Config) SetStorageBackend(b string) {
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.StorageBackend = b
	}
}

// EffectiveStorageBackend resolves the active mode ("tmp", "disk", "s3", or "hybrid"),
// routing disk/hybrid configs with empty/"none"/"off"/"disabled" CACHE_DIR to "tmp"/"s3" respectively.
func (c *Config) EffectiveStorageBackend() string {
	cacheDir := strings.ToLower(strings.TrimSpace(c.GetCacheDir()))
	cacheDisabled := cacheDir == "" || cacheDir == "none" || cacheDir == "off" || cacheDir == "disabled"

	switch c.GetStorageBackend() {
	case "s3":
		return "s3"
	case "tmp":
		return "tmp"
	case "hybrid":
		if cacheDisabled {
			return "s3"
		}
		return "hybrid"
	default:
		if cacheDisabled {
			return "tmp"
		}
		return "disk"
	}
}

// UsesCacheDir reports whether this runtime actually reads or writes persistent CACHE_DIR files.
func (c *Config) UsesCacheDir() bool {
	eff := c.EffectiveStorageBackend()
	return eff == "disk" || eff == "hybrid"
}

// clampConcurrency normalizes concurrency values to [1, 16], falling back when val <= 0.
func clampConcurrency(val, fallback int) int {
	if val <= 0 {
		return fallback
	}
	if val > 16 {
		return 16
	}
	return val
}

func (c *Config) GetS3Config() S3Config {
	if c == nil {
		return S3Config{
			ChunkSize:            64 * 1024 * 1024,
			UploadConcurrency:    4,
			RecomposeConcurrency: 6,
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	cfg := c.S3Config
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 64 * 1024 * 1024
	}
	cfg.UploadConcurrency = clampConcurrency(cfg.UploadConcurrency, 4)
	cfg.RecomposeConcurrency = clampConcurrency(cfg.RecomposeConcurrency, 6)
	return cfg
}

func (c *Config) SetS3Config(s3Cfg S3Config) {
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.S3Config = s3Cfg
	}
}

func (c *Config) GetCacheMaxIdleAge() time.Duration {
	if c == nil {
		return 720 * time.Hour
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CacheMaxIdleAge
}

func (c *Config) SetCacheMaxIdleAge(val time.Duration) {
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.CacheMaxIdleAge = val
	}
}

func (c *Config) GetCacheEvictInterval() time.Duration {
	if c == nil {
		return 6 * time.Hour
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.CacheEvictInterval <= 0 {
		return 6 * time.Hour
	}
	return c.CacheEvictInterval
}

func (c *Config) SetCacheEvictInterval(val time.Duration) {
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.CacheEvictInterval = val
	}
}

func (c *Config) GetDispatcharrURL() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.DispatcharrURL
}

func (c *Config) SetDispatcharrURL(val string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.DispatcharrURL = val
}

func (c *Config) GetPrehydrateCapacityReservation() int {
	if c == nil {
		return 1
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.PrehydrateCapacityReservation <= 0 {
		return 1
	}
	return c.PrehydrateCapacityReservation
}

func (c *Config) SetPrehydrateCapacityReservation(val int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PrehydrateCapacityReservation = val
}

func (c *Config) GetDispatcharrPollInterval() time.Duration {
	if c == nil {
		return 5 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.DispatcharrPollInterval <= 0 {
		return 5 * time.Second
	}
	return c.DispatcharrPollInterval
}

func (c *Config) GetPrehydrateYieldBackoff() time.Duration {
	if c == nil {
		return 30 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.PrehydrateYieldBackoff <= 0 {
		return 30 * time.Second
	}
	return c.PrehydrateYieldBackoff
}

func (c *Config) GetEvictYieldedParts() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.EvictYieldedParts
}

func (c *Config) SetEvictYieldedParts(val bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.EvictYieldedParts = val
}

func (c *Config) GetEnableEpisodePrehydration() bool {
	if c == nil {
		return true
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.EnableEpisodePrehydration
}

func (c *Config) SetEnableEpisodePrehydration(val bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.EnableEpisodePrehydration = val
}

func (c *Config) GetMinReadThresholdBytes() int64 {
	if c == nil {
		return 5 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MinReadThresholdBytes <= 0 {
		return 5 * 1024 * 1024
	}
	return c.MinReadThresholdBytes
}

func (c *Config) SetMinReadThresholdBytes(val int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MinReadThresholdBytes = val
}

func (c *Config) GetMaxProbeBuffers() int {
	if c == nil {
		return 128
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.MaxProbeBuffers <= 0 {
		return 128
	}
	return c.MaxProbeBuffers
}

func (c *Config) SetMaxProbeBuffers(val int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MaxProbeBuffers = val
}

var GlobalConfig *Config

func parseConfigFromEnv() *Config {
	cfg := &Config{
		Port:                    getEnv("PORT", ":8080"),
		CacheDir:                getEnv("CACHE_DIR", "./cache/files"),
		TmpDir:                  getEnv("TMP_DIR", "./cache/tmp"),
		StateDir:                getEnv("STATE_DIR", ""),
		CacheMaxBytes:           getEnvSize("CACHE_MAX_BYTES", 50*1024*1024*1024),           // 50GB
		CacheMaxItemPct:         getEnvPercent("CACHE_MAX_ITEM_PCT", 50),
		CacheMinDiskFreeBytes:   getEnvSize("CACHE_MIN_DISK_FREE_BYTES", 10*1024*1024*1024), // 10GB
		TmpFileMaxBytes:         getEnvSize("TMP_FILE_MAX_BYTES", 512*1024*1024),            // 512MB
		TmpMinDiskFreeBytes:     getEnvSize("TMP_MIN_DISK_FREE_BYTES", 1*1024*1024*1024),    // 1GB
		CacheRevalidateInterval: getEnvDuration("CACHE_REVALIDATE_INTERVAL", 1*time.Hour),
		SchedulerMinTime:        getEnvDuration("SCHEDULER_MIN_TIME", 2*time.Second),
		UpstreamReadTimeout:     getEnvDuration("UPSTREAM_READ_TIMEOUT", 0),
		MaxUpstreamWorkers:      getEnvInt("MAX_UPSTREAM_WORKERS", 2),
		ClientHydrationWait:     getEnvDuration("CLIENT_HYDRATION_WAIT", 2*time.Second),
		ProbeFailureCooldown:    getEnvDuration("PROBE_FAILURE_COOLDOWN", 10*time.Minute),
		ProbeTransientCooldown:  getEnvDuration("PROBE_TRANSIENT_COOLDOWN", 30*time.Second),
		UpstreamUserAgent:       getEnv("UPSTREAM_USER_AGENT", "VLC/3.0.20 LibVLC/3.0.20"),
		UpstreamUserAgentMode:   getEnv("UPSTREAM_USER_AGENT_MODE", "smart"),
		MaxUpstreamRetries:      getEnvInt("MAX_UPSTREAM_RETRIES", 0),
		StarvationStallTimeout:  getEnvDuration("STARVATION_STALL_TIMEOUT", 35*time.Second),
		StallTeardownCooldown:   getEnvDuration("STALL_TEARDOWN_COOLDOWN", 5*time.Second),
		LogVerbose:              getEnvBool("LOG_VERBOSE", false),
		WritebackBufferSize:     getEnvSize("WRITEBACK_BUFFER_SIZE", 64*1024*1024), // 64MB
		StorageBackend:            getEnv("STORAGE_BACKEND", "disk"),
		NASSequentialStage:        getEnv("NAS_SEQUENTIAL_STAGE", "auto"),
		HybridDemoteHighWatermark: getEnvPercentClamped("HYBRID_DEMOTE_HIGH_WATERMARK", 80, 1, 100),
		HybridDemoteLowWatermark:  getEnvPercentClamped("HYBRID_DEMOTE_LOW_WATERMARK", 70, 0, 100),
		hybridLowWatermarkSet:     true,
		HybridEagerS3Backup:       getEnvBool("HYBRID_EAGER_S3_BACKUP", false),
		S3Config: S3Config{
			Endpoint:             getEnv("S3_ENDPOINT", ""),
			Bucket:               getEnv("S3_BUCKET", ""),
			Region:               getEnv("S3_REGION", "us-east-1"),
			AccessKey:            getEnv("S3_ACCESS_KEY", ""),
			SecretKey:            getEnv("S3_SECRET_KEY", ""),
			UsePathStyle:         getEnvBool("S3_USE_PATH_STYLE", true),
			DisableSSL:           getEnvBool("S3_DISABLE_SSL", false),
			ChunkSize:            getEnvSize("S3_CHUNK_SIZE", 64*1024*1024),
			UploadConcurrency:    clampConcurrency(getEnvInt("S3_UPLOAD_CONCURRENCY", 4), 4),
			RecomposeConcurrency: clampConcurrency(getEnvInt("S3_RECOMPOSE_CONCURRENCY", 6), 6),
		},
		CacheMaxIdleAge:               getEnvDuration("CACHE_MAX_IDLE_AGE", 720*time.Hour),
		CacheEvictInterval:            getEnvDuration("CACHE_EVICT_INTERVAL", 6*time.Hour),
		DispatcharrURL:                getEnv("DISPATCHARR_URL", ""),
		PrehydrateCapacityReservation: getEnvInt("PREHYDRATE_CAPACITY_RESERVATION", 1),
		DispatcharrPollInterval:       getEnvDuration("DISPATCHARR_POLL_INTERVAL", 5*time.Second),
		PrehydrateYieldBackoff:        getEnvDuration("PREHYDRATE_YIELD_BACKOFF", 30*time.Second),
		EvictYieldedParts:             getEnvBool("EVICT_YIELDED_PARTS", false),
		EnableEpisodePrehydration:     getEnvBool("ENABLE_EPISODE_PREHYDRATION", true),
		MinReadThresholdBytes:         getEnvSize("MIN_READ_THRESHOLD_BYTES", 5*1024*1024), // 5MB
		MaxProbeBuffers:               getEnvInt("MAX_PROBE_BUFFERS", 128),
	}

	// Format port if missing colon
	if !strings.HasPrefix(cfg.Port, ":") {
		cfg.Port = ":" + cfg.Port
	}

	// Allowed hosts parsing
	hosts := getEnv("ALLOWED_HOSTS", "")
	if hosts != "" {
		parts := strings.Split(hosts, ",")
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				cfg.AllowedHosts = append(cfg.AllowedHosts, trimmed)
			}
		}
	}
	return cfg
}

// detectLegacyConfigWarnings inspects process environment variables for removed, renamed,
// or deprecated values and returns startup warning messages so operators can clean up their config.
func detectLegacyConfigWarnings() []string {
	var warnings []string
	renamedOrRemoved := []struct {
		env     string
		message string
	}{
		{"BLOCK_SIZE", "BLOCK_SIZE has been removed (continuous byte-range engine replaced fixed blocks); remove it from your config"},
		{"SCHEDULER_MIN_BYTES", "SCHEDULER_MIN_BYTES has been removed (use SCHEDULER_MIN_TIME instead); remove it from your config"},
		{"MIN_DISK_FREE_BYTES", "MIN_DISK_FREE_BYTES was renamed to CACHE_MIN_DISK_FREE_BYTES and is ignored; update your config"},
		{"S3_SCRATCH_MAX_BYTES", "S3_SCRATCH_MAX_BYTES was renamed to TMP_FILE_MAX_BYTES and is ignored; update your config"},
		{"S3_SCRATCH_MIN_FREE_BYTES", "S3_SCRATCH_MIN_FREE_BYTES was renamed to TMP_MIN_DISK_FREE_BYTES and is ignored; update your config"},
		{"S3_MAX_IDLE_AGE", "S3_MAX_IDLE_AGE was renamed to CACHE_MAX_IDLE_AGE and is ignored; update your config"},
		{"S3_EVICT_INTERVAL", "S3_EVICT_INTERVAL was renamed to CACHE_EVICT_INTERVAL and is ignored; update your config"},
	}
	for _, item := range renamedOrRemoved {
		if val, ok := os.LookupEnv(item.env); ok && strings.TrimSpace(val) != "" {
			warnings = append(warnings, item.message)
		}
	}

	if val, ok := os.LookupEnv("STORAGE_BACKEND"); ok && strings.TrimSpace(val) != "" {
		norm := strings.ToLower(strings.TrimSpace(val))
		if norm == "local" {
			warnings = append(warnings, `STORAGE_BACKEND="local" is removed; set STORAGE_BACKEND="disk" instead`)
		} else if norm != "tmp" && norm != "disk" && norm != "s3" && norm != "hybrid" {
			warnings = append(warnings, "STORAGE_BACKEND="+val+` is invalid; valid values are "tmp", "disk", "s3", or "hybrid"`)
		}
	}

	if val, ok := os.LookupEnv("NAS_SEQUENTIAL_STAGE"); ok && strings.TrimSpace(val) != "" {
		norm := strings.ToLower(strings.TrimSpace(val))
		if norm != "auto" && norm != "true" && norm != "false" {
			warnings = append(warnings, "NAS_SEQUENTIAL_STAGE="+val+` is invalid; valid values are "auto", "true", or "false"`)
		}
	}

	if val, ok := os.LookupEnv("UPSTREAM_USER_AGENT_MODE"); ok && strings.TrimSpace(val) != "" {
		norm := strings.ToLower(strings.TrimSpace(val))
		switch norm {
		case "override":
			warnings = append(warnings, `UPSTREAM_USER_AGENT_MODE="override" is removed; use UPSTREAM_USER_AGENT_MODE="force" instead`)
		case "disabled":
			warnings = append(warnings, `UPSTREAM_USER_AGENT_MODE="disabled" is removed; use UPSTREAM_USER_AGENT_MODE="passthrough" instead`)
		case "smart", "force", "passthrough":
		default:
			warnings = append(warnings, "UPSTREAM_USER_AGENT_MODE="+val+` is invalid; valid values are "smart", "force", or "passthrough"`)
		}
	}

	return warnings
}

func LoadConfig() {
	for _, w := range detectLegacyConfigWarnings() {
		log.Printf("[Config] WARNING: %s", w)
	}
	cfg := parseConfigFromEnv()
	GlobalConfig = cfg
	s3Cfg := cfg.GetS3Config()
	log.Printf("[Config] Loaded configuration: Port=%s, CacheDir=%s, TmpDir=%s, StateDir=%s, StorageBackend=%s (Effective=%s), NASSequentialStage=%s, HybridDemoteHigh=%d%%, HybridDemoteLow=%d%%, HybridEagerS3Backup=%v, DispatcharrURL=%s, MaxCache=%dGB, CacheMaxItemPct=%d%%, CacheMinDiskFree=%dGB, TmpFileMax=%dMB, TmpMinDiskFree=%dMB, CacheMaxIdleAge=%s, CacheEvictInterval=%s, S3UploadConcurrency=%d, S3RecomposeConcurrency=%d, UpstreamReadTimeout=%s, MaxUpstreamRetries=%d, MaxUpstreamWorkers=%d, ClientHydrationWait=%s, ProbeFailureCooldown=%s, ProbeTransientCooldown=%s, SchedulerMinTime=%s, UpstreamUserAgent=%q, UpstreamUserAgentMode=%s, MinReadThreshold=%dMB, MaxProbeBuffers=%d",
		cfg.Port,
		cfg.CacheDir,
		cfg.TmpDir,
		cfg.GetStateDir(),
		cfg.GetStorageBackend(),
		cfg.EffectiveStorageBackend(),
		cfg.GetNASSequentialStage(),
		cfg.GetHybridDemoteHighWatermark(),
		cfg.GetHybridDemoteLowWatermark(),
		cfg.GetHybridEagerS3Backup(),
		cfg.GetDispatcharrURL(),
		cfg.CacheMaxBytes/(1024*1024*1024),
		cfg.GetCacheMaxItemPct(),
		cfg.GetCacheMinDiskFreeBytes()/(1024*1024*1024),
		cfg.GetTmpFileMaxBytes()/(1024*1024),
		cfg.GetTmpMinDiskFreeBytes()/(1024*1024),
		cfg.GetCacheMaxIdleAge(),
		cfg.GetCacheEvictInterval(),
		s3Cfg.UploadConcurrency,
		s3Cfg.RecomposeConcurrency,
		cfg.UpstreamReadTimeout,
		cfg.MaxUpstreamRetries,
		cfg.MaxUpstreamWorkers,
		cfg.ClientHydrationWait,
		cfg.ProbeFailureCooldown,
		cfg.ProbeTransientCooldown,
		cfg.SchedulerMinTime,
		cfg.GetUpstreamUserAgent(),
		cfg.GetUpstreamUserAgentMode(),
		cfg.GetMinReadThresholdBytes()/(1024*1024),
		cfg.GetMaxProbeBuffers(),
	)
}

func getEnvPercentClamped(key string, fallback, minVal, maxVal int) int {
	valStr, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(valStr), "%"))
	if trimmed == "" {
		return fallback
	}
	val, err := strconv.Atoi(trimmed)
	if err != nil {
		log.Printf("[Config] Warning: Invalid percentage value for %s: %s. Using default %d%%", key, valStr, fallback)
		return fallback
	}
	if val < minVal {
		return fallback
	}
	if val > maxVal {
		return maxVal
	}
	return val
}

func getEnvPercent(key string, fallback int) int {
	return normalizeCacheMaxItemPct(getEnvPercentClamped(key, fallback, 0, 100))
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

func parseSizeString(valStr string) (int64, error) {
	valStr = strings.TrimSpace(strings.ToLower(valStr))
	if valStr == "" || valStr == "0" || valStr == "none" || valStr == "disabled" || valStr == "off" {
		return 0, nil
	}

	var multiplier int64 = 1
	var numStr string

	if strings.HasSuffix(valStr, "tb") || strings.HasSuffix(valStr, "t") {
		multiplier = 1024 * 1024 * 1024 * 1024
		numStr = strings.TrimSuffix(strings.TrimSuffix(valStr, "tb"), "t")
	} else if strings.HasSuffix(valStr, "gb") || strings.HasSuffix(valStr, "g") {
		multiplier = 1024 * 1024 * 1024
		numStr = strings.TrimSuffix(strings.TrimSuffix(valStr, "gb"), "g")
	} else if strings.HasSuffix(valStr, "mb") || strings.HasSuffix(valStr, "m") {
		multiplier = 1024 * 1024
		numStr = strings.TrimSuffix(strings.TrimSuffix(valStr, "mb"), "m")
	} else if strings.HasSuffix(valStr, "kb") || strings.HasSuffix(valStr, "k") {
		multiplier = 1024
		numStr = strings.TrimSuffix(strings.TrimSuffix(valStr, "kb"), "k")
	} else if strings.HasSuffix(valStr, "b") {
		multiplier = 1
		numStr = strings.TrimSuffix(valStr, "b")
	} else {
		multiplier = 1
		numStr = valStr
	}

	numStr = strings.TrimSpace(numStr)
	val, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, err
	}

	return val * multiplier, nil
}

func getEnvSize(key string, fallback int64) int64 {
	valStr, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	val, err := parseSizeString(valStr)
	if err != nil {
		log.Printf("[Config] Warning: Invalid size value for %s: %s. Using default %d", key, valStr, fallback)
		return fallback
	}
	return val
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	valStr, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	val, err := time.ParseDuration(valStr)
	if err != nil {
		log.Printf("[Config] Warning: Invalid value for %s: %s. Using default %s", key, valStr, fallback)
		return fallback
	}
	return val
}

func getEnvBool(key string, fallback bool) bool {
	valStr, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	valStr = strings.TrimSpace(strings.ToLower(valStr))
	return valStr == "true" || valStr == "1" || valStr == "yes" || valStr == "on"
}

func getEnvInt(key string, fallback int) int {
	valStr, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		log.Printf("[Config] Warning: Invalid integer value for %s: %s. Using default %d", key, valStr, fallback)
		return fallback
	}
	return val
}
