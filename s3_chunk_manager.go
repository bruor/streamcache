package main

import (
	"context"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// S3ChunkManager manages progressive chunk uploads to S3, enforces scratch disk backpressure, and coordinates recomposition
type S3ChunkManager struct {
	key         string
	state       *FileCacheState
	storage     StorageBackend
	chunkSize   int64
	totalSize   int64
	totalParts  int
	isMultipart bool

	mu          sync.RWMutex
	uploaded    map[int]bool
	uploading   map[int]bool
	recomposing bool
	recomposed  bool

	// In-flight scratch budget & socket backpressure
	maxInFlightBytes int64
	minFreeDiskBytes int64
	inFlightBytes    int64
	budgetMu         sync.Mutex
	budgetChan       chan struct{}

	activeUploads int32
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
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

// NewS3ChunkManager creates a new chunk manager for an active hydration state
func NewS3ChunkManager(state *FileCacheState, storage StorageBackend, chunkSize int64) *S3ChunkManager {
	totalSize := state.TotalSize
	dynChunkSize, isMultipart := CalculateDynamicChunkConfig(totalSize, chunkSize)

	totalParts := int((totalSize + dynChunkSize - 1) / dynChunkSize)
	if totalParts <= 0 {
		totalParts = 1
	}
	if !isMultipart {
		totalParts = 1
	}

	maxInFlight := int64(2 * 1024 * 1024 * 1024) // 2GB default
	minFreeDisk := int64(1 * 1024 * 1024 * 1024) // 1GB default
	if GlobalConfig != nil {
		s3Cfg := GlobalConfig.GetS3Config()
		if s3Cfg.ScratchMaxBytes > 0 {
			maxInFlight = s3Cfg.ScratchMaxBytes
		}
		if s3Cfg.ScratchMinFreeBytes > 0 {
			minFreeDisk = s3Cfg.ScratchMinFreeBytes
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &S3ChunkManager{
		key:              state.Key,
		state:            state,
		storage:          storage,
		chunkSize:        dynChunkSize,
		totalSize:        totalSize,
		totalParts:       totalParts,
		isMultipart:      isMultipart,
		maxInFlightBytes: maxInFlight,
		minFreeDiskBytes: minFreeDisk,
		budgetChan:       make(chan struct{}, 1),
		uploaded:         make(map[int]bool),
		uploading:        make(map[int]bool),
		ctx:              ctx,
		cancel:           cancel,
	}
}

// IsMultipart returns whether this hydration uses S3 multipart upload
func (m *S3ChunkManager) IsMultipart() bool {
	if m == nil {
		return false
	}
	return m.isMultipart
}

// WaitScratchBudget pauses the caller if in-flight un-uploaded bytes exceed budget or disk free space is low
func (m *S3ChunkManager) WaitScratchBudget(ctx context.Context) error {
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

		diskFree, err := GetDiskFreeSpace(m.state.PartPath)
		diskOK := err != nil || diskFree >= uint64(m.minFreeDiskBytes)
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

// AddInFlightBytes increments the recorded un-uploaded scratch bytes
func (m *S3ChunkManager) AddInFlightBytes(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.budgetMu.Lock()
	m.inFlightBytes += n
	m.budgetMu.Unlock()
}

// SubInFlightBytes decrements the un-uploaded scratch bytes and signals waiting workers
func (m *S3ChunkManager) SubInFlightBytes(n int64) {
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

// TotalParts returns the total calculated chunk count
func (m *S3ChunkManager) TotalParts() int {
	return m.totalParts
}

// ChunkSize returns the chunk size
func (m *S3ChunkManager) ChunkSize() int64 {
	return m.chunkSize
}

// IsChunkUploaded returns true if the specified chunk is safely in S3
func (m *S3ChunkManager) IsChunkUploaded(chunkIndex int) bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.uploaded[chunkIndex]
}

// CheckAndTriggerUploads inspects RangeSet to see if any new chunks can be uploaded
func (m *S3ChunkManager) CheckAndTriggerUploads() {
	if m == nil || m.storage == nil {
		return
	}

	m.mu.Lock()
	if m.recomposed {
		m.mu.Unlock()
		return
	}

	if !m.isMultipart {
		m.mu.Unlock()
		if m.state.RangeSet.HasAvailable(0, m.totalSize) {
			m.AttemptRecompose()
		}
		return
	}

	var candidates []int
	for i := 0; i < m.totalParts; i++ {
		if !m.uploaded[i] && !m.uploading[i] {
			chunkStart := int64(i) * m.chunkSize
			chunkLen := m.chunkSize
			if chunkStart+chunkLen > m.totalSize {
				chunkLen = m.totalSize - chunkStart
			}

			if m.state.RangeSet.HasAvailable(chunkStart, chunkLen) {
				m.uploading[i] = true
				candidates = append(candidates, i)
			}
		}
	}
	m.mu.Unlock()

	for _, idx := range candidates {
		m.wg.Add(1)
		atomic.AddInt32(&m.activeUploads, 1)
		go m.uploadWorker(idx)
	}
}

func (m *S3ChunkManager) uploadWorker(chunkIndex int) {
	defer m.wg.Done()
	defer atomic.AddInt32(&m.activeUploads, -1)

	chunkStart := int64(chunkIndex) * m.chunkSize
	chunkLen := m.chunkSize
	if chunkStart+chunkLen > m.totalSize {
		chunkLen = m.totalSize - chunkStart
	}

	// Open read handle to local scratch file
	f, err := os.Open(m.state.PartPath)
	if err != nil {
		log.Printf("[S3ChunkManager] Failed to open local scratch for chunk %d of %s: %v", chunkIndex, m.key, err)
		m.mu.Lock()
		m.uploading[chunkIndex] = false
		m.mu.Unlock()
		return
	}
	defer f.Close()

	// Retry up to 3 times with exponential backoff
	var uploadErr error
	for attempt := 1; attempt <= 3; attempt++ {
		sectionReader := io.NewSectionReader(f, chunkStart, chunkLen)
		uploadErr = m.storage.UploadChunk(m.ctx, m.key, chunkIndex, sectionReader, chunkLen)
		if uploadErr == nil || m.ctx.Err() != nil {
			break
		}
		backoff := time.Duration(1<<(attempt-1)) * 500 * time.Millisecond
		log.Printf("[S3ChunkManager] S3 UploadChunk failed for %s chunk %d (attempt %d/3): %v. Retrying in %s...",
			m.key, chunkIndex, attempt, uploadErr, backoff)
		select {
		case <-time.After(backoff):
		case <-m.ctx.Done():
			return
		}
	}

	if uploadErr != nil {
		if m.ctx.Err() != nil {
			return
		}
		log.Printf("[S3ChunkManager] S3 UploadChunk permanently failed for %s chunk %d after 3 attempts: %v", m.key, chunkIndex, uploadErr)
		m.mu.Lock()
		m.uploading[chunkIndex] = false
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	m.uploaded[chunkIndex] = true
	m.uploading[chunkIndex] = false
	completedCount := len(m.uploaded)
	m.mu.Unlock()

	// 1. Deallocate physical disk blocks (Linux hole punching)
	_ = PunchHole(f.Fd(), chunkStart, chunkLen)

	// 2. Evict clean pages from host OS cache for this uploaded chunk
	FadviseDontNeed(f.Fd(), chunkStart, chunkLen)

	// 3. Release scratch budget and signal download workers
	m.SubInFlightBytes(chunkLen)

	log.Printf("[S3ChunkManager] Successfully committed chunk %d/%d (%.2f MB) to S3 for %s",
		chunkIndex+1, m.totalParts, float64(chunkLen)/(1024*1024), m.key)

	// If all chunks uploaded, attempt recomposition
	if completedCount == m.totalParts {
		m.AttemptRecompose()
	}
}

// IsRecomposed returns true if the monolithic S3 object has been sealed
func (m *S3ChunkManager) IsRecomposed() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.recomposed
}

// AttemptRecompose stitches uploaded chunks into the monolithic S3 object
func (m *S3ChunkManager) AttemptRecompose() {
	m.mu.Lock()
	if m.recomposing || m.recomposed {
		m.mu.Unlock()
		return
	}
	if m.isMultipart && len(m.uploaded) < m.totalParts {
		m.mu.Unlock()
		return
	}
	m.recomposing = true
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		meta := FileMeta{
			URL:           m.state.URL,
			TotalSize:     m.state.TotalSize,
			ContentType:   m.state.ContentType,
			ETag:          m.state.ETag,
			LastModified:  m.state.LastModified,
			LastValidated: m.state.LastValidated,
			Completed:     true,
		}

		var err error
		if !m.isMultipart {
			log.Printf("[S3ChunkManager] Non-multipart asset (%.2f MB). Committing direct to S3 for %s...",
				float64(m.totalSize)/(1024*1024), m.key)
			err = m.storage.CommitLocalFile(m.ctx, m.key, m.state.PartPath, meta)
		} else {
			log.Printf("[S3ChunkManager] All %d chunks uploaded to S3. Starting server-side recomposition for %s...", m.totalParts, m.key)
			err = m.storage.Recompose(m.ctx, m.key, m.totalParts, meta)
		}

		if err != nil {
			log.Printf("[S3ChunkManager] Server-side recomposition/commit failed for %s: %v", m.key, err)
			m.mu.Lock()
			m.recomposing = false
			m.mu.Unlock()
			return
		}

		m.mu.Lock()
		m.recomposed = true
		m.recomposing = false
		m.mu.Unlock()

		log.Printf("[S3ChunkManager] Server-side recomposition SUCCESS for %s. Monolithic object sealed.", m.key)

		// Wake up any waiting client readers
		m.state.Lock.Lock()
		for _, ch := range m.state.Clients {
			safeSignal(ch)
		}
		m.state.Lock.Unlock()
	}()
}

// Close cancels in-flight chunk uploads and waits for workers to exit
func (m *S3ChunkManager) Close() {
	if m == nil {
		return
	}
	m.cancel()
	select {
	case m.budgetChan <- struct{}{}:
	default:
	}
	m.wg.Wait()
}
