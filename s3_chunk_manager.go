package main

import (
	"context"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
)

// S3ChunkManager manages progressive chunk uploads to S3 and coordinates recomposition
type S3ChunkManager struct {
	key        string
	state      *FileCacheState
	storage    StorageBackend
	chunkSize  int64
	totalSize  int64
	totalParts int

	mu        sync.RWMutex
	uploaded  map[int]bool
	uploading map[int]bool
	reposed   bool

	activeUploads int32
	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

// NewS3ChunkManager creates a new chunk manager for an active hydration state
func NewS3ChunkManager(state *FileCacheState, storage StorageBackend, chunkSize int64) *S3ChunkManager {
	if chunkSize <= 0 {
		chunkSize = 64 * 1024 * 1024
	}

	totalSize := state.TotalSize
	totalParts := int((totalSize + chunkSize - 1) / chunkSize)
	if totalParts <= 0 {
		totalParts = 1
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &S3ChunkManager{
		key:        state.Key,
		state:      state,
		storage:    storage,
		chunkSize:  chunkSize,
		totalSize:  totalSize,
		totalParts: totalParts,
		uploaded:   make(map[int]bool),
		uploading:  make(map[int]bool),
		ctx:        ctx,
		cancel:     cancel,
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
	if m.reposed {
		m.mu.Unlock()
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

	sectionReader := io.NewSectionReader(f, chunkStart, chunkLen)

	err = m.storage.UploadChunk(m.ctx, m.key, chunkIndex, sectionReader, chunkLen)
	if err != nil {
		if m.ctx.Err() != nil {
			return
		}
		log.Printf("[S3ChunkManager] S3 UploadChunk failed for %s chunk %d: %v", m.key, chunkIndex, err)
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

	// Evict clean pages from host OS cache for this uploaded chunk
	FadviseDontNeed(f.Fd(), chunkStart, chunkLen)

	log.Printf("[S3ChunkManager] Successfully committed chunk %d/%d (%.2f MB) to S3 for %s",
		chunkIndex+1, m.totalParts, float64(chunkLen)/(1024*1024), m.key)

	// If all chunks uploaded, attempt recomposition
	if completedCount == m.totalParts {
		m.AttemptRecompose()
	}
}

// AttemptRecompose stitches uploaded chunks into the monolithic S3 object
func (m *S3ChunkManager) AttemptRecompose() {
	m.mu.Lock()
	if m.reposed || len(m.uploaded) < m.totalParts {
		m.mu.Unlock()
		return
	}
	m.reposed = true
	m.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		log.Printf("[S3ChunkManager] All %d chunks uploaded to S3. Starting server-side recomposition for %s...", m.totalParts, m.key)

		meta := FileMeta{
			URL:           m.state.URL,
			TotalSize:     m.state.TotalSize,
			ContentType:   m.state.ContentType,
			ETag:          m.state.ETag,
			LastModified:  m.state.LastModified,
			LastValidated: m.state.LastValidated,
			Completed:     true,
		}

		err := m.storage.Recompose(m.ctx, m.key, m.totalParts, meta)
		if err != nil {
			log.Printf("[S3ChunkManager] Server-side recomposition failed for %s: %v", m.key, err)
			m.mu.Lock()
			m.reposed = false
			m.mu.Unlock()
			return
		}

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
	m.wg.Wait()
}
