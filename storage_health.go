package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// ErrStorageCircuitOpen indicates the requested persistent storage tier circuit breaker is open.
var ErrStorageCircuitOpen = errors.New("storage tier circuit breaker open")

// GlobalStorageHealth tracks host NAS and S3 tier availability using non-blocking watchdogs.
var GlobalStorageHealth *StorageHealthMonitor

var globalStorageHealthMu sync.RWMutex

func getGlobalStorageHealth() *StorageHealthMonitor {
	globalStorageHealthMu.RLock()
	defer globalStorageHealthMu.RUnlock()
	return GlobalStorageHealth
}

func setGlobalStorageHealth(m *StorageHealthMonitor) {
	globalStorageHealthMu.Lock()
	defer globalStorageHealthMu.Unlock()
	GlobalStorageHealth = m
}

// StorageHealthSnapshot describes current circuit-breaker status for dashboard and diagnostics.
type StorageHealthSnapshot struct {
	NASMonitored   bool   `json:"nas_monitored"`
	NASHealthy     bool   `json:"nas_healthy"`
	NASCircuitOpen bool   `json:"nas_circuit_open"`
	NASLastError   string `json:"nas_last_error,omitempty"`
	S3Monitored    bool   `json:"s3_monitored"`
	S3Healthy      bool   `json:"s3_healthy"`
	S3CircuitOpen  bool   `json:"s3_circuit_open"`
	S3LastError    string `json:"s3_last_error,omitempty"`
}

// IsNASHealthy reports whether local/NAS CACHE_DIR storage is currently healthy.
func IsNASHealthy() bool {
	if m := getGlobalStorageHealth(); m != nil {
		return m.IsNASHealthy()
	}
	return true
}

// IsS3Healthy reports whether S3 storage is currently healthy.
func IsS3Healthy() bool {
	if m := getGlobalStorageHealth(); m != nil {
		return m.IsS3Healthy()
	}
	return true
}

// RecordNASFailure marks the NAS circuit open on unexpected I/O errors.
func RecordNASFailure(err error) {
	if m := getGlobalStorageHealth(); m != nil {
		m.RecordNASFailure(err)
	}
}

// RecordNASSuccess clears any open NAS circuit breaker state.
func RecordNASSuccess() {
	if m := getGlobalStorageHealth(); m != nil {
		m.RecordNASSuccess()
	}
}

// RecordS3Failure marks the S3 circuit open on unexpected backend errors.
func RecordS3Failure(err error) {
	if m := getGlobalStorageHealth(); m != nil {
		m.RecordS3Failure(err)
	}
}

// RecordS3Success clears any open S3 circuit breaker state.
func RecordS3Success() {
	if m := getGlobalStorageHealth(); m != nil {
		m.RecordS3Success()
	}
}

// HasHealthyPersistentTier reports whether at least one persistent destination tier can accept data.
func HasHealthyPersistentTier(cfg *Config, storage StorageBackend) bool {
	mode := ""
	if storage != nil {
		mode = storage.Type()
	} else if cfg != nil {
		mode = cfg.EffectiveStorageBackend()
	}

	switch mode {
	case "tmp":
		return false
	case "disk":
		if cfg != nil && !cfg.UsesCacheDir() {
			return false
		}
		return IsNASHealthy()
	case "s3":
		return IsS3Healthy()
	case "hybrid":
		nasOK := IsNASHealthy()
		if cfg != nil && !cfg.UsesCacheDir() {
			nasOK = false
		}
		return nasOK || IsS3Healthy()
	default:
		if cfg != nil && !cfg.UsesCacheDir() {
			return false
		}
		return IsNASHealthy()
	}
}

// StorageHealthMonitor runs bounded non-blocking health probes for NAS/disk and S3 tiers.
type StorageHealthMonitor struct {
	cfg          *Config
	storage      StorageBackend
	cacheDir     string
	nasMonitored bool
	s3Monitored  bool

	mu               sync.RWMutex
	nasOpen          bool
	nasLastErr       string
	nasOpenedAt      time.Time
	nasProbeInFlight bool
	nasProbeTimeout  time.Duration
	nasProbeInterval time.Duration
	nasProbeFn       func(dir string) error

	s3Open          bool
	s3LastErr       string
	s3OpenedAt      time.Time
	s3OpenCooldown  time.Duration
	s3ProbeTimeout  time.Duration
	s3ProbeInterval time.Duration
	s3ProbeInFlight bool
	s3ProbeFn       func(ctx context.Context) error

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func extractS3Backend(storage StorageBackend) *S3StorageBackend {
	switch s := storage.(type) {
	case *S3StorageBackend:
		return s
	case *TieredStorageBackend:
		return s.S3()
	default:
		return nil
	}
}

// NewStorageHealthMonitor creates a StorageHealthMonitor for the active storage topology.
func NewStorageHealthMonitor(cfg *Config, storage StorageBackend) *StorageHealthMonitor {
	mode := ""
	if storage != nil {
		mode = storage.Type()
	} else if cfg != nil {
		mode = cfg.EffectiveStorageBackend()
	}

	cacheDir := ""
	if cfg != nil && cfg.UsesCacheDir() {
		cacheDir = cfg.GetCacheDir()
	}
	if cacheDir == "" {
		switch b := storage.(type) {
		case *LocalStorageBackend:
			cacheDir = b.cacheDir
		case *TieredStorageBackend:
			if b.Local() != nil {
				cacheDir = b.Local().cacheDir
			}
		}
	}

	nasMonitored := (mode == "disk" || mode == "hybrid" || mode == "") && cacheDir != ""
	s3Backend := extractS3Backend(storage)
	s3Monitored := mode == "s3" || mode == "hybrid" || s3Backend != nil

	ctx, cancel := context.WithCancel(context.Background())
	return &StorageHealthMonitor{
		cfg:              cfg,
		storage:          storage,
		cacheDir:         cacheDir,
		nasMonitored:     nasMonitored,
		s3Monitored:      s3Monitored,
		nasProbeTimeout:  3 * time.Second,
		nasProbeInterval: 10 * time.Second,
		nasProbeFn: func(dir string) error {
			if _, err := os.Stat(dir); err != nil {
				return err
			}
			_, err := GetDiskFreeSpace(dir)
			return err
		},
		s3OpenCooldown:  30 * time.Second,
		s3ProbeTimeout:  5 * time.Second,
		s3ProbeInterval: 10 * time.Second,
		s3ProbeFn: func(probeCtx context.Context) error {
			if s3Backend == nil {
				return nil
			}
			return s3Backend.Ping(probeCtx)
		},
		ctx:    ctx,
		cancel: cancel,
	}
}

// Start begins periodic background NAS and S3 recovery probing.
func (m *StorageHealthMonitor) Start() {
	if m == nil || (!m.nasMonitored && !m.s3Monitored) {
		return
	}
	m.wg.Add(1)
	go m.watchdogLoop()
}

// Stop terminates the background watchdog loop cleanly without blocking on stalled OS probes.
func (m *StorageHealthMonitor) Stop() {
	if m == nil {
		return
	}
	m.stopOnce.Do(func() {
		m.cancel()
	})
	m.wg.Wait()
}

func (m *StorageHealthMonitor) watchdogLoop() {
	defer m.wg.Done()

	m.mu.RLock()
	interval := m.nasProbeInterval
	if m.s3ProbeInterval > 0 && (interval <= 0 || m.s3ProbeInterval < interval) {
		interval = m.s3ProbeInterval
	}
	m.mu.RUnlock()
	if interval <= 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if m.nasMonitored {
				_ = m.ProbeNASOnce(m.ctx)
			}
			if m.s3Monitored {
				m.mu.RLock()
				s3Open := m.s3Open
				openedAt := m.s3OpenedAt
				cooldown := m.s3OpenCooldown
				m.mu.RUnlock()
				if s3Open && (cooldown <= 0 || time.Since(openedAt) >= cooldown) {
					_ = m.ProbeS3Once(m.ctx)
				}
			}
		}
	}
}

// IsNASHealthy reports whether the NAS circuit breaker is currently closed.
func (m *StorageHealthMonitor) IsNASHealthy() bool {
	if m == nil {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.nasOpen
}

// IsS3Healthy reports whether the S3 circuit breaker is currently closed.
func (m *StorageHealthMonitor) IsS3Healthy() bool {
	if m == nil {
		return true
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return !m.s3Open
}

// Snapshot returns an atomic copy of current NAS and S3 health states.
func (m *StorageHealthMonitor) Snapshot() StorageHealthSnapshot {
	if m == nil {
		return StorageHealthSnapshot{NASHealthy: true, S3Healthy: true}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return StorageHealthSnapshot{
		NASMonitored:   m.nasMonitored,
		NASHealthy:     !m.nasOpen,
		NASCircuitOpen: m.nasOpen,
		NASLastError:   m.nasLastErr,
		S3Monitored:    m.s3Monitored,
		S3Healthy:      !m.s3Open,
		S3CircuitOpen:  m.s3Open,
		S3LastError:    m.s3LastErr,
	}
}

func shouldIgnoreStorageErr(err error, checkS3NotFound bool) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrStorageCircuitOpen) || os.IsNotExist(err) {
		return true
	}
	if checkS3NotFound && isS3NotFoundError(err) {
		return true
	}
	return false
}

// RecordNASFailure opens the NAS circuit breaker on unexpected I/O failures.
func (m *StorageHealthMonitor) RecordNASFailure(err error) {
	if m == nil || !m.nasMonitored || shouldIgnoreStorageErr(err, false) {
		return
	}
	m.mu.Lock()
	newlyOpened := !m.nasOpen
	m.nasOpen = true
	m.nasLastErr = err.Error()
	m.nasOpenedAt = time.Now()
	m.mu.Unlock()

	if newlyOpened {
		log.Printf("[StorageHealth] NAS/disk circuit breaker OPENED (%s): %v", m.cacheDir, err)
	}
	m.onOutageTripped()
}

// RecordNASSuccess closes the NAS circuit breaker if previously open.
func (m *StorageHealthMonitor) RecordNASSuccess() {
	if m == nil || !m.nasMonitored {
		return
	}
	m.mu.Lock()
	wasOpen := m.nasOpen
	if wasOpen {
		m.nasOpen = false
		m.nasLastErr = ""
		m.nasOpenedAt = time.Time{}
	}
	m.mu.Unlock()

	if wasOpen {
		log.Printf("[StorageHealth] NAS/disk circuit breaker CLOSED (recovered: %s)", m.cacheDir)
		m.notifyRecovery("nas")
	}
}

// RecordS3Failure opens the S3 circuit breaker on unexpected S3 errors (ignoring 404/NoSuchKey/Cancel).
func (m *StorageHealthMonitor) RecordS3Failure(err error) {
	if m == nil || !m.s3Monitored || shouldIgnoreStorageErr(err, true) {
		return
	}
	m.mu.Lock()
	newlyOpened := !m.s3Open
	m.s3Open = true
	m.s3LastErr = err.Error()
	m.s3OpenedAt = time.Now()
	m.mu.Unlock()

	if newlyOpened {
		log.Printf("[StorageHealth] S3 circuit breaker OPENED: %v", err)
	}
	m.onOutageTripped()
}

// RecordS3Success closes the S3 circuit breaker if previously open.
func (m *StorageHealthMonitor) RecordS3Success() {
	if m == nil || !m.s3Monitored {
		return
	}
	m.mu.Lock()
	wasOpen := m.s3Open
	if wasOpen {
		m.s3Open = false
		m.s3LastErr = ""
		m.s3OpenedAt = time.Time{}
	}
	m.mu.Unlock()

	if wasOpen {
		log.Printf("[StorageHealth] S3 circuit breaker CLOSED (recovered)")
		m.notifyRecovery("s3")
	}
}

// ProbeNASOnce runs a bounded-timeout probe against CACHE_DIR in an isolated goroutine
// so kernel D-state hangs never block the watchdog loop or request goroutines.
func (m *StorageHealthMonitor) ProbeNASOnce(ctx context.Context) error {
	if m == nil || !m.nasMonitored || m.cacheDir == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.nasProbeInFlight {
		newlyOpened := !m.nasOpen
		m.nasOpen = true
		m.nasLastErr = "nas health probe still blocked (possible D-state hang)"
		if m.nasOpenedAt.IsZero() {
			m.nasOpenedAt = time.Now()
		}
		m.mu.Unlock()
		if newlyOpened {
			log.Printf("[StorageHealth] NAS probe still stuck in flight; keeping circuit breaker OPEN (%s)", m.cacheDir)
		}
		m.onOutageTripped()
		return errors.New("nas health probe still blocked (possible D-state hang)")
	}
	m.nasProbeInFlight = true
	dir := m.cacheDir
	timeout := m.nasProbeTimeout
	probeFn := m.nasProbeFn
	m.mu.Unlock()

	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	if probeFn == nil {
		probeFn = func(d string) error {
			if _, err := os.Stat(d); err != nil {
				return err
			}
			_, err := GetDiskFreeSpace(d)
			return err
		}
	}

	resultCh := make(chan error, 1)
	go func() {
		err := probeFn(dir)
		var recovered, tripped, newlyOpened bool
		m.mu.Lock()
		m.nasProbeInFlight = false
		if err == nil {
			wasOpen := m.nasOpen
			m.nasOpen = false
			m.nasLastErr = ""
			m.nasOpenedAt = time.Time{}
			recovered = wasOpen
		} else {
			newlyOpened = !m.nasOpen
			m.nasOpen = true
			m.nasLastErr = err.Error()
			m.nasOpenedAt = time.Now()
			tripped = true
		}
		m.mu.Unlock()

		if recovered {
			log.Printf("[StorageHealth] NAS/disk watchdog probe recovered (%s)", dir)
			m.notifyRecovery("nas")
		} else if tripped {
			if newlyOpened {
				log.Printf("[StorageHealth] NAS/disk watchdog probe failed (%s): %v", dir, err)
			}
			m.onOutageTripped()
		}
		resultCh <- err
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-resultCh:
		return err
	case <-timer.C:
		err := fmt.Errorf("nas health probe timed out after %s (possible D-state hang)", timeout)
		m.mu.Lock()
		newlyOpened := !m.nasOpen
		m.nasOpen = true
		m.nasLastErr = err.Error()
		m.nasOpenedAt = time.Now()
		m.mu.Unlock()
		if newlyOpened {
			log.Printf("[StorageHealth] %v (%s)", err, dir)
		}
		m.onOutageTripped()
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ProbeS3Once checks S3 connectivity with a bounded timeout and closes/keeps open the S3 breaker.
func (m *StorageHealthMonitor) ProbeS3Once(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	if !m.s3Monitored || m.s3ProbeFn == nil || m.s3ProbeInFlight {
		m.mu.Unlock()
		return nil
	}
	m.s3ProbeInFlight = true
	probeFn := m.s3ProbeFn
	timeout := m.s3ProbeTimeout
	m.mu.Unlock()

	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := probeFn(probeCtx)

	var recovered, tripped, newlyOpened bool
	m.mu.Lock()
	m.s3ProbeInFlight = false
	if err == nil {
		wasOpen := m.s3Open
		m.s3Open = false
		m.s3LastErr = ""
		m.s3OpenedAt = time.Time{}
		recovered = wasOpen
	} else {
		newlyOpened = !m.s3Open
		m.s3Open = true
		m.s3LastErr = err.Error()
		m.s3OpenedAt = time.Now()
		tripped = true
	}
	m.mu.Unlock()

	if recovered {
		log.Printf("[StorageHealth] S3 watchdog probe recovered")
		m.notifyRecovery("s3")
	} else if tripped {
		if newlyOpened {
			log.Printf("[StorageHealth] S3 watchdog probe failed: %v", err)
		}
		m.onOutageTripped()
	}
	return err
}

func (m *StorageHealthMonitor) notifyRecovery(tier string) {
	if pm := GlobalPrehydrationManager; pm != nil {
		pm.ClearTierContention(tier)
		pm.NotifyStorageRecovered()
	}
	if d := getGlobalHybridDemoter(); d != nil {
		d.TriggerWatermarkDemotion()
	}
	if tier == "s3" {
		if idx := getGlobalCacheIndex(); idx != nil {
			idx.TriggerPendingRecompose()
		}
	}
	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
}

func (m *StorageHealthMonitor) onOutageTripped() {
	if !HasHealthyPersistentTier(m.cfg, m.storage) {
		if pm := GlobalPrehydrationManager; pm != nil {
			pm.PauseForStorageOutageWith(m.cfg, m.storage, GlobalRegistry)
		}
	}
	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
}
