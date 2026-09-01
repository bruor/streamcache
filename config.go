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

var GlobalConfig *Config

func LoadConfig() {
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
