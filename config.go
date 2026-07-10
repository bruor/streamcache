package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                     string
	CacheDir                 string
	TmpDir                   string
	BlockSize                int64
	CacheMaxBytes            int64
	MinDiskFreeBytes         int64
	CacheRevalidateInterval  time.Duration
	AllowedHosts             []string
	SchedulerMinBlocks       int
	MaxPrefetchTailBlocks    int
	SchedulerMinTime         time.Duration
	DisableFadvise           bool
	UpstreamReadTimeout      time.Duration
	MaxUpstreamWorkers       int
	ClientHydrationWait      time.Duration
	ProbeFailureCooldown     time.Duration
	ProbeTransientCooldown   time.Duration
}

var GlobalConfig *Config

func LoadConfig() {
	cfg := &Config{
		Port:                    getEnv("PORT", ":8080"),
		CacheDir:                getEnv("CACHE_DIR", "./cache/files"),
		TmpDir:                  getEnv("TMP_DIR", "./cache/tmp"),
		BlockSize:               getEnvSize("BLOCK_SIZE", 4*1024*1024), // 4MB
		CacheMaxBytes:           getEnvSize("CACHE_MAX_BYTES", 50*1024*1024*1024), // 50GB
		MinDiskFreeBytes:        getEnvSize("MIN_DISK_FREE_BYTES", 10*1024*1024*1024), // 10GB
		CacheRevalidateInterval: getEnvDuration("CACHE_REVALIDATE_INTERVAL", 1*time.Hour),
		SchedulerMinBlocks:      getEnvInt("SCHEDULER_MIN_BLOCKS", 4), // 4 blocks (16MB default)
		MaxPrefetchTailBlocks:   getEnvInt("MAX_PREFETCH_TAILBLOCKS", 10), // Max 10 blocks (40MB default)
		SchedulerMinTime:        getEnvDuration("SCHEDULER_MIN_TIME", 2*time.Second),
		DisableFadvise:          getEnvBool("DISABLE_FADVISE", false),
		UpstreamReadTimeout:     getEnvDuration("UPSTREAM_READ_TIMEOUT", 30*time.Second),
		MaxUpstreamWorkers:      getEnvInt("MAX_UPSTREAM_WORKERS", 2),
		ClientHydrationWait:     getEnvDuration("CLIENT_HYDRATION_WAIT", 2*time.Second),
		ProbeFailureCooldown:    getEnvDuration("PROBE_FAILURE_COOLDOWN", 10*time.Minute),
		ProbeTransientCooldown:  getEnvDuration("PROBE_TRANSIENT_COOLDOWN", 30*time.Second),
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
	log.Printf("[Config] Loaded configuration: Port=%s, CacheDir=%s, BlockSize=%dMB, MaxCache=%dGB, MinDiskFree=%dGB, UpstreamReadTimeout=%s, DisableFadvise=%t, MaxUpstreamWorkers=%d, ClientHydrationWait=%s, ProbeFailureCooldown=%s, ProbeTransientCooldown=%s, SchedulerMinBlocks=%d, MaxPrefetchTailBlocks=%d",
		cfg.Port,
		cfg.CacheDir,
		cfg.BlockSize/(1024*1024),
		cfg.CacheMaxBytes/(1024*1024*1024),
		cfg.MinDiskFreeBytes/(1024*1024*1024),
		cfg.UpstreamReadTimeout,
		cfg.DisableFadvise,
		cfg.MaxUpstreamWorkers,
		cfg.ClientHydrationWait,
		cfg.ProbeFailureCooldown,
		cfg.ProbeTransientCooldown,
		cfg.SchedulerMinBlocks,
		cfg.MaxPrefetchTailBlocks,
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
