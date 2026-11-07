package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	mu                      sync.RWMutex
	Port                    string
	CacheDir                string
	TmpDir                  string
	CacheMaxBytes           int64
	MinDiskFreeBytes        int64
	CacheRevalidateInterval time.Duration
	AllowedHosts            []string
	SchedulerMinTime        time.Duration
	UpstreamReadTimeout     time.Duration
	MaxUpstreamWorkers      int
	ClientHydrationWait     time.Duration
	ProbeFailureCooldown    time.Duration
	ProbeTransientCooldown  time.Duration
	UpstreamUserAgent       string
	UpstreamUserAgentMode   string
	MaxUpstreamRetries      int
	StarvationStallTimeout  time.Duration
	LogVerbose              bool
	WritebackBufferSize     int64
	DispatcharrURL                string
	PrehydrateCapacityReservation int
	DispatcharrPollInterval       time.Duration
	PrehydrateYieldBackoff        time.Duration
	EvictYieldedParts             bool
	EnableEpisodePrehydration     bool
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
		return 30 * time.Second
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.StarvationStallTimeout <= 0 {
		return 30 * time.Second
	}
	return c.StarvationStallTimeout
}

func (c *Config) GetCacheMaxBytes() int64 {
	if c == nil {
		return 50 * 1024 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.CacheMaxBytes
}

func (c *Config) GetMinDiskFreeBytes() int64 {
	if c == nil {
		return 10 * 1024 * 1024 * 1024
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.MinDiskFreeBytes
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
	if mode == "override" {
		return "force"
	}
	if mode == "disabled" {
		return "passthrough"
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

func (c *Config) GetPrefetchCapacityReservation() int {
	return c.GetPrehydrateCapacityReservation()
}

func (c *Config) SetPrehydrateCapacityReservation(val int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.PrehydrateCapacityReservation = val
}

func (c *Config) SetPrefetchCapacityReservation(val int) {
	c.SetPrehydrateCapacityReservation(val)
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

func (c *Config) GetPrefetchYieldBackoff() time.Duration {
	return c.GetPrehydrateYieldBackoff()
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

func (c *Config) GetEnableEpisodePrefetch() bool {
	return c.GetEnableEpisodePrehydration()
}

func (c *Config) SetEnableEpisodePrehydration(val bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.EnableEpisodePrehydration = val
}

func (c *Config) SetEnableEpisodePrefetch(val bool) {
	c.SetEnableEpisodePrehydration(val)
}

var GlobalConfig *Config

func parseConfigFromEnv() *Config {
	cfg := &Config{
		Port:                    getEnv("PORT", ":8080"),
		CacheDir:                getEnv("CACHE_DIR", "./cache/files"),
		TmpDir:                  getEnv("TMP_DIR", "./cache/tmp"),
		CacheMaxBytes:           getEnvSize("CACHE_MAX_BYTES", 50*1024*1024*1024),     // 50GB
		MinDiskFreeBytes:        getEnvSize("MIN_DISK_FREE_BYTES", 10*1024*1024*1024), // 10GB
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
		StarvationStallTimeout:  getEnvDuration("STARVATION_STALL_TIMEOUT", 30*time.Second),
		LogVerbose:              getEnvBool("LOG_VERBOSE", false),
		WritebackBufferSize:     getEnvSize("WRITEBACK_BUFFER_SIZE", 64*1024*1024), // 64MB
		DispatcharrURL:                getEnv("DISPATCHARR_URL", ""),
		PrehydrateCapacityReservation: getEnvInt("PREHYDRATE_CAPACITY_RESERVATION", getEnvInt("PREFETCH_CAPACITY_RESERVATION", 1)),
		DispatcharrPollInterval:       getEnvDuration("DISPATCHARR_POLL_INTERVAL", 5*time.Second),
		PrehydrateYieldBackoff:        getEnvDuration("PREHYDRATE_YIELD_BACKOFF", getEnvDuration("PREFETCH_YIELD_BACKOFF", 30*time.Second)),
		EvictYieldedParts:             getEnvBool("EVICT_YIELDED_PARTS", false),
		EnableEpisodePrehydration:     getEnvBool("ENABLE_EPISODE_PREHYDRATION", getEnvBool("ENABLE_EPISODE_PREFETCH", true)),
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

func LoadConfig() {
	cfg := parseConfigFromEnv()
	GlobalConfig = cfg
	log.Printf("[Config] Loaded configuration: Port=%s, CacheDir=%s, MaxCache=%dGB, MinDiskFree=%dGB, UpstreamReadTimeout=%s, MaxUpstreamRetries=%d, MaxUpstreamWorkers=%d, ClientHydrationWait=%s, ProbeFailureCooldown=%s, ProbeTransientCooldown=%s, SchedulerMinTime=%s, UpstreamUserAgent=%q, UpstreamUserAgentMode=%s",
		cfg.Port,
		cfg.CacheDir,
		cfg.CacheMaxBytes/(1024*1024*1024),
		cfg.MinDiskFreeBytes/(1024*1024*1024),
		cfg.UpstreamReadTimeout,
		cfg.MaxUpstreamRetries,
		cfg.MaxUpstreamWorkers,
		cfg.ClientHydrationWait,
		cfg.ProbeFailureCooldown,
		cfg.ProbeTransientCooldown,
		cfg.SchedulerMinTime,
		cfg.GetUpstreamUserAgent(),
		cfg.GetUpstreamUserAgentMode(),
	)
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
