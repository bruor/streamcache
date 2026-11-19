package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// ChunkTarget abstracts the progressive chunk destination for ScratchChunkManager
// (S3 multipart chunks or cross-filesystem NAS staging files).
type ChunkTarget interface {
	Name() string
	IsSequential() bool
	Concurrency() int
	SupportsMultipart(totalSize, configuredChunkSize int64) (chunkSize int64, isMultipart bool)
	PushChunk(ctx context.Context, key string, chunkIndex int, chunkStart int64, r io.Reader, size int64) error
	Commit(ctx context.Context, key string, isMultipart bool, totalParts int, localPartPath string, meta FileMeta, onProgress CommitProgressFunc) error
	ReadCommittedChunk(ctx context.Context, key string, chunkIndex int, chunkStart, offsetInChunk, length int64) (io.ReadCloser, error)
	Cleanup(ctx context.Context, key string) error
}

// S3ChunkTarget adapts a remote StorageBackend (e.g. S3StorageBackend) to ChunkTarget.
type S3ChunkTarget struct {
	storage StorageBackend
}

// NewS3ChunkTarget wraps storage as a ChunkTarget for remote multipart uploads.
func NewS3ChunkTarget(storage StorageBackend) *S3ChunkTarget {
	return &S3ChunkTarget{storage: storage}
}

func (t *S3ChunkTarget) Name() string       { return "s3" }
func (t *S3ChunkTarget) IsSequential() bool { return false }

func (t *S3ChunkTarget) Concurrency() int {
	conc := 4
	if GlobalConfig != nil {
		if sc := GlobalConfig.GetS3Config().UploadConcurrency; sc > 0 {
			conc = sc
		}
	}
	if s3b, ok := t.storage.(*S3StorageBackend); ok && s3b.UploadConcurrency() > 0 {
		conc = s3b.UploadConcurrency()
	}
	return clampConcurrency(conc, 4)
}

func (t *S3ChunkTarget) SupportsMultipart(totalSize, configuredChunkSize int64) (int64, bool) {
	return CalculateDynamicChunkConfig(totalSize, configuredChunkSize)
}

func (t *S3ChunkTarget) PushChunk(ctx context.Context, key string, chunkIndex int, _ int64, r io.Reader, size int64) error {
	return t.storage.UploadChunk(ctx, key, chunkIndex, r, size)
}

func (t *S3ChunkTarget) Commit(ctx context.Context, key string, isMultipart bool, totalParts int, localPartPath string, meta FileMeta, onProgress CommitProgressFunc) error {
	if !isMultipart {
		err := t.storage.CommitLocalFile(ctx, key, localPartPath, meta)
		if err == nil && onProgress != nil {
			onProgress("finalizing", 1, 1)
		}
		return err
	}
	commitCtx := WithCommitProgress(ctx, onProgress)
	return t.storage.Recompose(commitCtx, key, totalParts, meta)
}

func (t *S3ChunkTarget) ReadCommittedChunk(ctx context.Context, key string, chunkIndex int, _ int64, offsetInChunk, length int64) (io.ReadCloser, error) {
	return t.storage.OpenChunkRange(ctx, key, chunkIndex, offsetInChunk, length)
}

func (t *S3ChunkTarget) Cleanup(ctx context.Context, key string) error {
	if cleaner, ok := t.storage.(ChunkCleanupBackend); ok {
		return cleaner.DeleteChunks(ctx, key)
	}
	return nil
}

// ScratchChunkManager manages progressive chunk uploads or NAS staging, enforces TMP_DIR scratch backpressure, and coordinates final commit.
type ScratchChunkManager struct {
	key         string
	state       *FileCacheState
	storage     StorageBackend
	target      ChunkTarget
	chunkSize   int64
	totalSize   int64
	totalParts  int
	isMultipart bool

	mu              sync.RWMutex
	uploaded        map[int]bool
	uploading       map[int]bool
	recomposing     bool
	recomposed      bool
	recomposeErr    error
	permanentErr    error
	commitStage     string
	commitCompleted int
	commitTotal     int

	// In-flight scratch budget & socket backpressure
	maxInFlightBytes        int64
	minFreeDiskBytes        int64
	defaultMinFreeDiskBytes int64
	inFlightBytes           int64
	budgetMu                sync.Mutex
	budgetChan              chan struct{}

	uploadSem        chan struct{}
	trackDiskFlushes atomic.Bool
	activeUploads    int32
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup // tracks chunk upload workers
	recomposeWg      sync.WaitGroup // tracks server-side recomposition / commit
}

// CalculateDynamicChunkConfig calculates the optimal chunk size and determines
// whether multipart upload or single-object upload should be used.
func CalculateDynamicChunkConfig(totalSize int64, configuredChunkSize int64) (chunkSize int64, isMultipart bool) {
	const minPartSize = 5 * 1024 * 1024 // 5 MB S3 minimum part limit
	const maxParts = 10000              // S3 maximum multipart limit

	if totalSize <= 0 {
		if configuredChunkSize <= 0 {
			configuredChunkSize = 64 * 1024 * 1024
		}
		return configuredChunkSize, true
	}

	// If configuredChunkSize is explicitly set smaller than 5 MB (e.g. in test suites / mocks),
	// honor the explicit chunk size and enable multipart testing.
	if configuredChunkSize > 0 && configuredChunkSize < minPartSize {
		return configuredChunkSize, true
	}

	// Rule 1: Files <= 5 MB cannot use S3 multipart upload in production (must use single PutObject)
	if totalSize <= minPartSize {
		return totalSize, false
	}

	// Default to configured chunk size
	chunkSize = configuredChunkSize
	if chunkSize < minPartSize {
		chunkSize = minPartSize
	}

	// Rule 2: Ensure total parts never exceed 10,000 (auto-scaled for large files > 640 GB)
	requiredMinPartSize := (totalSize + maxParts - 1) / maxParts
	if chunkSize < requiredMinPartSize {
		// Round up to nearest 1 MB boundary
		chunkSize = ((requiredMinPartSize + 1024*1024 - 1) / (1024 * 1024)) * (1024 * 1024)
	}

	return chunkSize, true
}

// NewScratchChunkManager creates a new chunk manager targeting the given StorageBackend.
func NewScratchChunkManager(state *FileCacheState, storage StorageBackend, chunkSize int64) *ScratchChunkManager {
	var target ChunkTarget
	if storage != nil {
		target = NewS3ChunkTarget(storage)
	}
	m := NewScratchChunkManagerWithTarget(state, target, chunkSize)
	m.storage = storage
	return m
}

// NewScratchChunkManagerWithTarget creates a chunk manager targeting an explicit ChunkTarget.
func NewScratchChunkManagerWithTarget(state *FileCacheState, target ChunkTarget, chunkSize int64) *ScratchChunkManager {
	totalSize := state.TotalSize
	var dynChunkSize int64
	var isMultipart bool
	if target != nil {
		dynChunkSize, isMultipart = target.SupportsMultipart(totalSize, chunkSize)
	} else {
		dynChunkSize, isMultipart = CalculateDynamicChunkConfig(totalSize, chunkSize)
	}
	if dynChunkSize <= 0 {
		dynChunkSize = totalSize
		if dynChunkSize <= 0 {
			dynChunkSize = 1
		}
	}

	totalParts := int((totalSize + dynChunkSize - 1) / dynChunkSize)
	if totalParts <= 0 || !isMultipart {
		totalParts = 1
	}

	maxInFlight := int64(512 * 1024 * 1024)    // 512MB default (TMP_FILE_MAX_BYTES)
	minFreeDisk := int64(1 * 1024 * 1024 * 1024) // 1GB default (TMP_MIN_DISK_FREE_BYTES)
	if GlobalConfig != nil {
		maxInFlight = GlobalConfig.GetTmpFileMaxBytes()
		minFreeDisk = EffectiveTmpMinDiskFreeBytes(GlobalConfig, GetStorageTopology())
	}

	uploadConc := 4
	if target != nil {
		uploadConc = target.Concurrency()
	}
	uploadConc = clampConcurrency(uploadConc, 4)
	if target != nil && target.IsSequential() {
		uploadConc = 1
	}
	if dynChunkSize > 0 && maxInFlight < 2*dynChunkSize {
		maxInFlight = 2 * dynChunkSize
	}

	var storage StorageBackend
	if s3t, ok := target.(*S3ChunkTarget); ok {
		storage = s3t.storage
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &ScratchChunkManager{
		key:                     state.Key,
		state:                   state,
		storage:                 storage,
		target:                  target,
		chunkSize:               dynChunkSize,
		totalSize:               totalSize,
		totalParts:              totalParts,
		isMultipart:             isMultipart,
		maxInFlightBytes:        maxInFlight,
		minFreeDiskBytes:        minFreeDisk,
		defaultMinFreeDiskBytes: minFreeDisk,
		budgetChan:              make(chan struct{}, 1),
		uploadSem:               make(chan struct{}, uploadConc),
		uploaded:                make(map[int]bool),
		uploading:               make(map[int]bool),
		ctx:                     ctx,
		cancel:                  cancel,
	}
}

// TargetName returns the active ChunkTarget identifier ("s3", "nas", or "").
func (m *ScratchChunkManager) TargetName() string {
	if m == nil || m.target == nil {
		return ""
	}
	return m.target.Name()
}

// ReadCommittedChunk reads a byte range from a chunk committed to the target.
func (m *ScratchChunkManager) ReadCommittedChunk(ctx context.Context, chunkIndex int, offsetInChunk, length int64) (io.ReadCloser, error) {
	if m == nil || m.target == nil {
		return nil, fmt.Errorf("no chunk target configured")
	}
	chunkStart := int64(chunkIndex) * m.chunkSize
	return m.target.ReadCommittedChunk(ctx, m.key, chunkIndex, chunkStart, offsetInChunk, length)
}

// Cleanup removes any staged/remote chunks owned by this chunk manager's target.
func (m *ScratchChunkManager) Cleanup(ctx context.Context) error {
	if m == nil || m.target == nil {
		return nil
	}
	return m.target.Cleanup(ctx, m.key)
}

// IsMultipart returns whether this hydration uses multipart chunked upload.
func (m *ScratchChunkManager) IsMultipart() bool {
	if m == nil {
		return false
	}
	return m.isMultipart
}

// WaitScratchBudget pauses the caller if in-flight un-uploaded bytes exceed budget or disk free space is low.
func (m *ScratchChunkManager) WaitScratchBudget(ctx context.Context) error {
	if m == nil {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			return m.ctx.Err()
		default:
		}

		m.budgetMu.Lock()
		inFlight := m.inFlightBytes
		m.budgetMu.Unlock()

		minFree := m.minFreeDiskBytes
		if minFree == m.defaultMinFreeDiskBytes && GlobalConfig != nil {
			minFree = EffectiveTmpMinDiskFreeBytes(GlobalConfig, GetStorageTopology())
		}
		diskPath := m.state.PartPath
		if dir := filepath.Dir(diskPath); dir != "" && dir != "." {
			diskPath = dir
		}
		diskFree, err := GetDiskFreeSpace(diskPath)
		diskOK := err != nil || diskFree >= uint64(minFree)
		budgetOK := inFlight < m.maxInFlightBytes

		if budgetOK && diskOK {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			return m.ctx.Err()
		case <-m.budgetChan:
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// AddInFlightBytes increments the recorded un-uploaded scratch bytes.
func (m *ScratchChunkManager) AddInFlightBytes(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.budgetMu.Lock()
	m.inFlightBytes += n
	m.budgetMu.Unlock()
}

// SubInFlightBytes decrements the un-uploaded scratch bytes and signals waiting workers.
func (m *ScratchChunkManager) SubInFlightBytes(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.budgetMu.Lock()
	m.inFlightBytes -= n
	if m.inFlightBytes < 0 {
		m.inFlightBytes = 0
	}
	m.budgetMu.Unlock()

	select {
	case m.budgetChan <- struct{}{}:
	default:
	}
}

// TotalParts returns the total calculated chunk count.
func (m *ScratchChunkManager) TotalParts() int {
	return m.totalParts
}

// ChunkSize returns the chunk size.
func (m *ScratchChunkManager) ChunkSize() int64 {
	return m.chunkSize
}

// IsChunkUploaded returns true if the specified chunk is safely stored remotely.
func (m *ScratchChunkManager) IsChunkUploaded(chunkIndex int) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.uploaded[chunkIndex]
}

// SetTrackDiskFlushes controls whether CheckAndTriggerUploads gates on DiskRangeSet
// (bytes flushed to disk by WritebackPipeline) vs RangeSet.
func (m *ScratchChunkManager) SetTrackDiskFlushes(enabled bool) {
	if m == nil {
		return
	}
	m.trackDiskFlushes.Store(enabled)
}

func (m *ScratchChunkManager) activeRangeSet() *RangeSet {
	if m.trackDiskFlushes.Load() && m.state.DiskRangeSet != nil {
		return m.state.DiskRangeSet
	}
	return m.state.RangeSet
}

// CheckAndTriggerUploads inspects RangeSet (or DiskRangeSet when pipelined) to see if any new chunks can be queued to the target.
func (m *ScratchChunkManager) CheckAndTriggerUploads() {
	if m == nil || m.target == nil {
		return
	}

	rs := m.activeRangeSet()
	sequential := m.target.IsSequential()

	m.mu.Lock()
	if m.recomposed || m.permanentErr != nil {
		m.mu.Unlock()
		return
	}

	if !m.isMultipart {
		m.mu.Unlock()
		if rs.HasAvailable(0, m.totalSize) {
			m.AttemptRecompose()
		}
		return
	}

	var candidates []int
	for i := 0; i < m.totalParts; i++ {
		if m.uploaded[i] {
			continue
		}
		if m.uploading[i] {
			if sequential {
				break
			}
			continue
		}
		chunkStart := int64(i) * m.chunkSize
		chunkLen := m.chunkSize
		if chunkStart+chunkLen > m.totalSize {
			chunkLen = m.totalSize - chunkStart
		}

		if rs.HasAvailable(chunkStart, chunkLen) {
			m.uploading[i] = true
			candidates = append(candidates, i)
			if sequential {
				break
			}
		} else if sequential {
			break
		}
	}
	m.mu.Unlock()

	for _, idx := range candidates {
		m.wg.Add(1)
		atomic.AddInt32(&m.activeUploads, 1)
		go m.uploadWorker(idx)
	}
}

func (m *ScratchChunkManager) uploadWorker(chunkIndex int) {
	defer m.wg.Done()
	defer atomic.AddInt32(&m.activeUploads, -1)

	select {
	case m.uploadSem <- struct{}{}:
	case <-m.ctx.Done():
		m.mu.Lock()
		m.uploading[chunkIndex] = false
		m.mu.Unlock()
		return
	}
	defer func() { <-m.uploadSem }()

	chunkStart := int64(chunkIndex) * m.chunkSize
	chunkLen := m.chunkSize
	if chunkStart+chunkLen > m.totalSize {
		chunkLen = m.totalSize - chunkStart
	}

	// Open read-write handle so fallocate(FALLOC_FL_PUNCH_HOLE) succeeds (O_RDONLY fails with EBADF)
	f, err := os.OpenFile(m.state.PartPath, os.O_RDWR, 0644)
	if err != nil {
		log.Printf("[ScratchChunkManager] Failed to open local scratch for chunk %d of %s: %v", chunkIndex, m.key, err)
		m.mu.Lock()
		m.uploading[chunkIndex] = false
		m.mu.Unlock()
		return
	}
	defer f.Close()

	// Retry up to 3 times with exponential backoff
	targetName := m.target.Name()
	var uploadErr error
	for attempt := 1; attempt <= 3; attempt++ {
		sectionReader := io.NewSectionReader(f, chunkStart, chunkLen)
		uploadErr = m.target.PushChunk(m.ctx, m.key, chunkIndex, chunkStart, sectionReader, chunkLen)
		if uploadErr == nil || m.ctx.Err() != nil {
			break
		}
		backoff := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
		log.Printf("[ScratchChunkManager] PushChunk (%s) failed for %s chunk %d (attempt %d/3): %v. Retrying in %s...",
			targetName, m.key, chunkIndex, attempt, uploadErr, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-m.ctx.Done():
			timer.Stop()
			m.mu.Lock()
			m.uploading[chunkIndex] = false
			m.mu.Unlock()
			return
		}
	}

	if uploadErr != nil {
		if m.ctx.Err() != nil {
			m.mu.Lock()
			m.uploading[chunkIndex] = false
			m.mu.Unlock()
			return
		}
		log.Printf("[ScratchChunkManager] PushChunk (%s) permanently failed for %s chunk %d after 3 attempts: %v",
			targetName, m.key, chunkIndex, uploadErr)
		m.mu.Lock()
		m.uploading[chunkIndex] = false
		if m.permanentErr == nil {
			m.permanentErr = uploadErr
		}
		m.mu.Unlock()
		m.cancel()
		select {
		case m.budgetChan <- struct{}{}:
		default:
		}
		target := m.target
		key := m.key
		go func() {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := target.Cleanup(delCtx, key); err != nil {
				log.Printf("[ScratchChunkManager] Async Cleanup (%s) failed for %s: %v", targetName, key, err)
			}
		}()
		return
	}

	m.mu.Lock()
	m.uploaded[chunkIndex] = true
	m.uploading[chunkIndex] = false
	completedCount := len(m.uploaded)
	m.mu.Unlock()

	// 1. Deallocate physical disk blocks (Linux hole punching)
	if punchErr := PunchHole(f.Fd(), chunkStart, chunkLen); punchErr != nil {
		log.Printf("[ScratchChunkManager] WARNING: PunchHole failed for %s chunk %d [%d, %d): %v",
			m.key, chunkIndex, chunkStart, chunkStart+chunkLen, punchErr)
	}

	// 2. Evict clean pages from host OS cache for this uploaded chunk
	FadviseDontNeed(f.Fd(), chunkStart, chunkLen)

	// 3. Release scratch budget and signal download workers
	m.SubInFlightBytes(chunkLen)

	log.Printf("[ScratchChunkManager] Successfully pushed chunk %d/%d (%.2f MB) to %s for %s",
		chunkIndex+1, m.totalParts, float64(chunkLen)/(1024*1024), targetName, m.key)

	// If all chunks pushed, attempt final commit/recomposition; otherwise pipeline next sequential chunk if applicable.
	if completedCount == m.totalParts {
		m.AttemptRecompose()
	} else if m.target.IsSequential() {
		m.CheckAndTriggerUploads()
	}
}

// IsRecomposed returns true if the monolithic S3 object has been sealed.
func (m *ScratchChunkManager) IsRecomposed() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.recomposed
}

// setCommitProgress records the active commit stage and item progress under lock.
func (m *ScratchChunkManager) setCommitProgress(stage string, completed, total int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.commitStage = stage
	m.commitCompleted = completed
	m.commitTotal = total
	m.mu.Unlock()
}

// GetCommitProgress returns the current S3 commit/recomposition stage and phase counts.
func (m *ScratchChunkManager) GetCommitProgress() *CommitProgress {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.recomposed {
		total := m.commitTotal
		if total <= 0 {
			total = 1
		}
		return &CommitProgress{Stage: "finalizing", Completed: total, Total: total}
	}

	if m.commitStage == "staging" || m.commitStage == "recomposing" || m.commitStage == "finalizing" {
		return &CommitProgress{
			Stage:     m.commitStage,
			Completed: m.commitCompleted,
			Total:     m.commitTotal,
		}
	}

	isNAS := m.target != nil && m.target.Name() == "nas"
	if m.isMultipart && m.totalParts > 0 {
		uploaded := len(m.uploaded)
		if uploaded < m.totalParts {
			stage := "uploading"
			if isNAS {
				stage = "staging"
			}
			return &CommitProgress{
				Stage:     stage,
				Completed: uploaded,
				Total:     m.totalParts,
			}
		}
		stage := "recomposing"
		if isNAS {
			stage = "finalizing"
		}
		return &CommitProgress{
			Stage:     stage,
			Completed: 0,
			Total:     m.totalParts,
		}
	}

	return &CommitProgress{
		Stage:     "finalizing",
		Completed: 0,
		Total:     1,
	}
}

// AttemptRecompose commits uploaded chunks or staged files to the target destination.
func (m *ScratchChunkManager) AttemptRecompose() {
	if m == nil || m.target == nil {
		return
	}
	isNAS := m.target.Name() == "nas"

	m.mu.Lock()
	if m.recomposing || m.recomposed || m.permanentErr != nil {
		m.mu.Unlock()
		return
	}
	if m.isMultipart && len(m.uploaded) < m.totalParts {
		m.mu.Unlock()
		return
	}
	m.recomposing = true
	m.recomposeErr = nil
	if m.isMultipart && !isNAS {
		m.commitStage = "recomposing"
		m.commitCompleted = 0
		m.commitTotal = m.totalParts
	} else {
		m.commitStage = "finalizing"
		m.commitCompleted = 0
		m.commitTotal = 1
	}
	m.mu.Unlock()

	m.recomposeWg.Add(1)
	go func() {
		defer m.recomposeWg.Done()

		meta := FileMeta{
			URL:           m.state.URL,
			TotalSize:     m.state.TotalSize,
			ContentType:   m.state.ContentType,
			ETag:          m.state.ETag,
			LastModified:  m.state.LastModified,
			LastValidated: m.state.LastValidated,
			Completed:     true,
		}

		targetName := m.target.Name()
		if !m.isMultipart {
			log.Printf("[ScratchChunkManager] Non-multipart asset (%.2f MB). Committing direct to %s for %s...",
				float64(m.totalSize)/(1024*1024), targetName, m.key)
		} else {
			log.Printf("[ScratchChunkManager] All %d chunks pushed to %s. Starting final commit for %s...", m.totalParts, targetName, m.key)
		}

		err := m.target.Commit(m.ctx, m.key, m.isMultipart, m.totalParts, m.state.PartPath, meta, m.setCommitProgress)
		if err == nil {
			m.mu.RLock()
			stage := m.commitStage
			total := m.commitTotal
			m.mu.RUnlock()
			if stage != "finalizing" {
				if total <= 0 {
					total = 1
				}
				m.setCommitProgress("finalizing", total, total)
			}
		}

		if err != nil {
			log.Printf("[ScratchChunkManager] Server-side recomposition/commit failed for %s: %v", m.key, err)
			m.mu.Lock()
			m.recomposing = false
			m.recomposeErr = err
			m.mu.Unlock()
			return
		}

		m.mu.Lock()
		m.recomposed = true
		m.recomposing = false
		m.recomposeErr = nil
		m.mu.Unlock()

		log.Printf("[ScratchChunkManager] Server-side recomposition SUCCESS for %s. Monolithic object sealed.", m.key)

		// Wake up any waiting client readers
		m.state.Lock.Lock()
		for _, ch := range m.state.Clients {
			safeSignal(ch)
		}
		m.state.Lock.Unlock()
	}()
}

// WaitAndRecompose triggers any remaining chunk uploads, waits for all uploads to finish,
// and waits for server-side recomposition (or direct CommitLocalFile) to seal the S3 object.
func (m *ScratchChunkManager) WaitAndRecompose(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if m.IsRecomposed() {
		return nil
	}

	waitWG := func(wg *sync.WaitGroup) error {
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-m.ctx.Done():
			m.mu.RLock()
			permErr := m.permanentErr
			m.mu.RUnlock()
			if permErr != nil {
				return permErr
			}
			return m.ctx.Err()
		}
	}

	// Ensure any remaining completed chunks (e.g. final chunk after pipeline flush) are uploaded
	m.CheckAndTriggerUploads()
	if err := waitWG(&m.wg); err != nil {
		return err
	}

	m.mu.RLock()
	permErr := m.permanentErr
	m.mu.RUnlock()
	if permErr != nil {
		return permErr
	}

	// If any multipart chunk failed transiently and isn't uploaded yet, trigger one retry pass
	if m.isMultipart {
		m.mu.RLock()
		uploadedCount := len(m.uploaded)
		m.mu.RUnlock()
		if uploadedCount < m.totalParts {
			m.CheckAndTriggerUploads()
			if err := waitWG(&m.wg); err != nil {
				return err
			}
			m.mu.RLock()
			uploadedCount = len(m.uploaded)
			permErr = m.permanentErr
			m.mu.RUnlock()
			if permErr != nil {
				return permErr
			}
			if uploadedCount < m.totalParts {
				return fmt.Errorf("incomplete S3 chunk uploads (%d/%d) for %s", uploadedCount, m.totalParts, m.key)
			}
		}
	}

	m.AttemptRecompose()
	if err := waitWG(&m.recomposeWg); err != nil {
		return err
	}

	m.mu.RLock()
	recomposed := m.recomposed
	err := m.recomposeErr
	m.mu.RUnlock()

	if !recomposed {
		if err != nil {
			return err
		}
		return fmt.Errorf("S3 recomposition did not complete for %s", m.key)
	}
	return nil
}

// Close cancels in-flight chunk uploads and waits for workers to exit.
func (m *ScratchChunkManager) Close() {
	if m == nil {
		return
	}
	m.cancel()
	select {
	case m.budgetChan <- struct{}{}:
	default:
	}
	m.wg.Wait()
	m.recomposeWg.Wait()
}
