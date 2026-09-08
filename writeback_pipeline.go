package main

import (
	"context"
	"log"
	"os"
	"sort"
	"sync"
	"sync/atomic"
)

const (
	// DefaultWritebackChunkSize is the size of each streaming network chunk
	DefaultWritebackChunkSize = 128 * 1024 // 128KB
	// DefaultWritebackSyncWindow is the threshold of written bytes before triggering SyncFileRange
	DefaultWritebackSyncWindow = 8 * 1024 * 1024 // 8MB
)

var chunkPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, DefaultWritebackChunkSize)
		return &b
	},
}

// pooledChunk represents a shared in-memory chunk with refcounting
type pooledChunk struct {
	offset   int64
	length   int
	bufPtr   *[]byte
	refCount int32
	seq      uint64
}

func newPooledChunk(offset int64, data []byte, refCount int32) *pooledChunk {
	bufPtr := chunkPool.Get().(*[]byte)
	if len(*bufPtr) < len(data) {
		b := make([]byte, len(data))
		bufPtr = &b
	}
	copy(*bufPtr, data)
	return &pooledChunk{
		offset:   offset,
		length:   len(data),
		bufPtr:   bufPtr,
		refCount: refCount,
	}
}

func (c *pooledChunk) release() {
	if atomic.AddInt32(&c.refCount, -1) == 0 {
		if c.bufPtr != nil {
			chunkPool.Put(c.bufPtr)
			c.bufPtr = nil
		}
	}
}

// LiveBuffer maintains an in-memory sliding window of recent chunks for zero-copy live-edge reads
type LiveBuffer struct {
	mu       sync.RWMutex
	chunks   []*pooledChunk
	maxBytes int64
	curBytes int64
	seqGen   uint64
}

// NewLiveBuffer creates a LiveBuffer capped at maxBytes
func NewLiveBuffer(maxBytes int64) *LiveBuffer {
	if maxBytes <= 0 {
		maxBytes = 64 * 1024 * 1024 // 64MB default
	}
	return &LiveBuffer{
		maxBytes: maxBytes,
		chunks:   make([]*pooledChunk, 0, 512),
	}
}

// Put adds a chunk to the LiveBuffer in sorted offset order and evicts old chunks if capacity exceeded
func (lb *LiveBuffer) Put(chunk *pooledChunk) {
	if lb == nil || chunk == nil {
		return
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.seqGen++
	chunk.seq = lb.seqGen

	// Find insertion point by offset (binary search)
	idx := sort.Search(len(lb.chunks), func(i int) bool {
		return lb.chunks[i].offset >= chunk.offset
	})

	if idx < len(lb.chunks) && lb.chunks[idx].offset == chunk.offset {
		// Replace existing chunk at same offset
		old := lb.chunks[idx]
		lb.curBytes -= int64(old.length)
		old.release()
		lb.chunks[idx] = chunk
	} else {
		// Insert at idx
		lb.chunks = append(lb.chunks, nil)
		copy(lb.chunks[idx+1:], lb.chunks[idx:])
		lb.chunks[idx] = chunk
	}
	lb.curBytes += int64(chunk.length)

	// Evict least recently used/added chunks if exceeding maxBytes
	for lb.curBytes > lb.maxBytes && len(lb.chunks) > 0 {
		minIdx := 0
		minSeq := atomic.LoadUint64(&lb.chunks[0].seq)
		for i := 1; i < len(lb.chunks); i++ {
			s := atomic.LoadUint64(&lb.chunks[i].seq)
			if s < minSeq {
				minSeq = s
				minIdx = i
			}
		}

		evicted := lb.chunks[minIdx]
		copy(lb.chunks[minIdx:], lb.chunks[minIdx+1:])
		lb.chunks[len(lb.chunks)-1] = nil
		lb.chunks = lb.chunks[:len(lb.chunks)-1]
		lb.curBytes -= int64(evicted.length)
		evicted.release()
	}
}

// ReadAt attempts to satisfy a read directly from in-memory LiveBuffer
func (lb *LiveBuffer) ReadAt(p []byte, off int64) (int, bool) {
	if lb == nil || len(p) == 0 {
		return 0, false
	}

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if len(lb.chunks) == 0 {
		return 0, false
	}

	// Binary search for chunk covering off
	// Find greatest chunk with offset <= off
	idx := sort.Search(len(lb.chunks), func(i int) bool {
		return lb.chunks[i].offset > off
	}) - 1

	if idx < 0 || idx >= len(lb.chunks) {
		return 0, false
	}

	totalCopied := 0
	reqOff := off
	dest := p

	for idx < len(lb.chunks) && len(dest) > 0 {
		c := lb.chunks[idx]
		cEnd := c.offset + int64(c.length)

		if reqOff < c.offset || reqOff >= cEnd {
			break // Non-contiguous or beyond chunk
		}

		chunkStart := reqOff - c.offset
		availInChunk := int64(c.length) - chunkStart
		toCopy := int64(len(dest))
		if toCopy > availInChunk {
			toCopy = availInChunk
		}

		// Refresh sequence number on read so active player locations are not evicted
		atomic.StoreUint64(&c.seq, atomic.AddUint64(&lb.seqGen, 1))

		copy(dest[:toCopy], (*c.bufPtr)[chunkStart:chunkStart+toCopy])
		dest = dest[toCopy:]
		reqOff += toCopy
		totalCopied += int(toCopy)
		idx++
	}

	if totalCopied > 0 {
		return totalCopied, true
	}
	return 0, false
}

// Clear releases all chunks currently retained in LiveBuffer
func (lb *LiveBuffer) Clear() {
	if lb == nil {
		return
	}

	lb.mu.Lock()
	defer lb.mu.Unlock()

	for _, c := range lb.chunks {
		if c != nil {
			c.release()
		}
	}
	lb.chunks = nil
	lb.curBytes = 0
}

// StorageSink represents an abstraction for writing media chunks
type StorageSink interface {
	WriteChunk(offset int64, data []byte) error
	FlushAndClose() error
}

type writeWindow struct {
	start int64
	end   int64
}

// WritebackPipeline coordinates decoupled disk writes and continuous writeback pacing
type WritebackPipeline struct {
	file       *os.File
	state      *FileCacheState
	liveBuffer *LiveBuffer
	queue      chan *pooledChunk
	mu         sync.RWMutex
	closed     bool
	wg         sync.WaitGroup
	fd         uintptr
	lastErr    error
	errMu      sync.Mutex

	// Writeback pacing tracking (Linux sync_file_range)
	windows          []writeWindow
	prevWindowOffset int64
	prevWindowLength int64
}

// NewWritebackPipeline initializes the pipeline and starts the storage consumer goroutine
func NewWritebackPipeline(file *os.File, state *FileCacheState, bufferBytes int64) *WritebackPipeline {
	if bufferBytes <= 0 {
		bufferBytes = 64 * 1024 * 1024 // 64MB default
	}

	queueCap := int(bufferBytes / DefaultWritebackChunkSize)
	if queueCap < 64 {
		queueCap = 64
	}

	var fd uintptr
	if file != nil {
		fd = file.Fd()
	}

	var lb *LiveBuffer
	if state != nil {
		state.Lock.Lock()
		if state.LiveBuffer == nil {
			state.LiveBuffer = NewLiveBuffer(bufferBytes)
		}
		lb = state.LiveBuffer
		state.Lock.Unlock()
	} else {
		lb = NewLiveBuffer(bufferBytes)
	}

	wp := &WritebackPipeline{
		file:       file,
		state:      state,
		liveBuffer: lb,
		queue:      make(chan *pooledChunk, queueCap),
		fd:         fd,
		windows:    make([]writeWindow, 0, 4),
	}

	wp.wg.Add(1)
	go wp.storageConsumer()

	return wp
}

// WriteChunk copies incoming slice into pool buffer, stores in LiveBuffer, and queues for storage
func (wp *WritebackPipeline) WriteChunk(offset int64, data []byte) error {
	return wp.WriteChunkContext(context.Background(), offset, data)
}

// WriteChunkContext adds context-aware cancellation to WriteChunk
func (wp *WritebackPipeline) WriteChunkContext(ctx context.Context, offset int64, data []byte) error {
	wp.mu.RLock()
	if wp.closed {
		wp.mu.RUnlock()
		return os.ErrClosed
	}

	wp.errMu.Lock()
	if wp.lastErr != nil {
		err := wp.lastErr
		wp.errMu.Unlock()
		wp.mu.RUnlock()
		return err
	}
	wp.errMu.Unlock()

	refCount := int32(1)
	if wp.liveBuffer != nil {
		refCount = 2
	}
	chunk := newPooledChunk(offset, data, refCount)

	// 1. Immediately store in LiveBuffer for zero-copy client playback
	if wp.liveBuffer != nil {
		wp.liveBuffer.Put(chunk)
	}

	// 2. Queue for asynchronous disk writing & kernel writeback
	select {
	case wp.queue <- chunk:
		wp.mu.RUnlock()
		return nil
	case <-ctx.Done():
		wp.mu.RUnlock()
		chunk.release() // release consumer's reference
		return ctx.Err()
	}
}

// storageConsumer runs as a dedicated consumer goroutine writing chunks to disk
func (wp *WritebackPipeline) storageConsumer() {
	defer wp.wg.Done()

	for chunk := range wp.queue {
		if wp.file != nil && chunk.bufPtr != nil {
			_, err := wp.file.WriteAt((*chunk.bufPtr)[:chunk.length], chunk.offset)
			if err != nil {
				log.Printf("[WritebackPipeline] WriteAt error at offset %d: %v", chunk.offset, err)
				wp.errMu.Lock()
				if wp.lastErr == nil {
					wp.lastErr = err
				}
				wp.errMu.Unlock()
			}
		}

		// Writeback pacing (Linux SyncFileRange + Fadvise)
		wp.paceWriteback(chunk.offset, int64(chunk.length))

		// Release chunk reference for consumer
		chunk.release()
	}

	// Final flush on consumer exit
	wp.flushFinalWriteback()
}

// paceWriteback accumulates written bytes into windows and paces asynchronous flush to physical disk
func (wp *WritebackPipeline) paceWriteback(offset int64, length int64) {
	if wp.fd == 0 {
		return
	}

	chunkEnd := offset + length
	matchedIdx := -1

	for i := range wp.windows {
		if wp.windows[i].end == offset {
			wp.windows[i].end = chunkEnd
			matchedIdx = i
			break
		} else if chunkEnd == wp.windows[i].start {
			wp.windows[i].start = offset
			matchedIdx = i
			break
		}
	}

	if matchedIdx == -1 {
		if len(wp.windows) >= 4 {
			oldest := wp.windows[0]
			wp.windows = wp.windows[1:]
			span := oldest.end - oldest.start
			if span > 0 {
				SyncFileRangeWrite(wp.fd, oldest.start, span)
				SyncFileRangeWaitAndEvict(wp.fd, oldest.start, span)
			}
		}
		wp.windows = append(wp.windows, writeWindow{start: offset, end: chunkEnd})
		matchedIdx = len(wp.windows) - 1
	}

	w := &wp.windows[matchedIdx]
	span := w.end - w.start
	if span >= DefaultWritebackSyncWindow {
		currOffset := w.start
		currLength := span

		// 1. Asynchronously initiate writeout of the completed window
		SyncFileRangeWrite(wp.fd, currOffset, currLength)

		// 2. Await hardware writeout completion of previous window and evict clean pages from host RAM
		if wp.prevWindowLength > 0 {
			SyncFileRangeWaitAndEvict(wp.fd, wp.prevWindowOffset, wp.prevWindowLength)
		}

		wp.prevWindowOffset = currOffset
		wp.prevWindowLength = currLength
		w.start = w.end
	}
}

// flushFinalWriteback cleanly flushes and evicts remaining written bytes
func (wp *WritebackPipeline) flushFinalWriteback() {
	if wp.fd == 0 {
		return
	}

	for _, w := range wp.windows {
		span := w.end - w.start
		if span > 0 {
			SyncFileRangeWrite(wp.fd, w.start, span)
			SyncFileRangeWaitAndEvict(wp.fd, w.start, span)
		}
	}
	wp.windows = nil

	if wp.prevWindowLength > 0 {
		SyncFileRangeWaitAndEvict(wp.fd, wp.prevWindowOffset, wp.prevWindowLength)
		wp.prevWindowLength = 0
	}
}

// FlushAndClose drains the queue, completes writebacks, and shuts down the pipeline
func (wp *WritebackPipeline) FlushAndClose() error {
	wp.mu.Lock()
	if wp.closed {
		wp.mu.Unlock()
		return nil
	}
	wp.closed = true
	close(wp.queue)
	wp.mu.Unlock()

	wp.wg.Wait()

	if wp.liveBuffer != nil {
		wp.liveBuffer.Clear()
	}

	wp.errMu.Lock()
	defer wp.errMu.Unlock()
	return wp.lastErr
}
