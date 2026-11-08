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

	seq := atomic.AddUint64(&lb.seqGen, 1)
	atomic.StoreUint64(&chunk.seq, seq)

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
		lb.curBytes += int64(chunk.length)
	} else if idx > 0 &&
		lb.chunks[idx-1].offset+int64(lb.chunks[idx-1].length) == chunk.offset &&
		lb.chunks[idx-1].length+chunk.length <= len(*lb.chunks[idx-1].bufPtr) &&
		(idx == len(lb.chunks) || lb.chunks[idx].offset >= chunk.offset+int64(chunk.length)) &&
		atomic.LoadInt32(&lb.chunks[idx-1].refCount) == 1 &&
		atomic.LoadInt32(&chunk.refCount) == 1 {
		// Coalesce contiguous chunk into existing 128KB buffer and return temporary chunk to pool
		prev := lb.chunks[idx-1]
		copy((*prev.bufPtr)[prev.length:prev.length+chunk.length], (*chunk.bufPtr)[:chunk.length])
		prev.length += chunk.length
		lb.curBytes += int64(chunk.length)
		atomic.StoreUint64(&prev.seq, seq)
		chunk.release()
	} else {
		// Insert at idx
		lb.chunks = append(lb.chunks, nil)
		copy(lb.chunks[idx+1:], lb.chunks[idx:])
		lb.chunks[idx] = chunk
		lb.curBytes += int64(chunk.length)
	}

	lb.evictLocked()
}

// PutSlice copies data directly into a coalesced 128KB chunk in LiveBuffer when contiguous,
// avoiding allocating a separate 128KB pooled buffer for small network reads.
func (lb *LiveBuffer) PutSlice(offset int64, data []byte, pinForWrite bool) writeReq {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	seq := atomic.AddUint64(&lb.seqGen, 1)

	idx := sort.Search(len(lb.chunks), func(i int) bool {
		return lb.chunks[i].offset >= offset
	})

	if idx > 0 && (idx == len(lb.chunks) || lb.chunks[idx].offset >= offset+int64(len(data))) {
		prev := lb.chunks[idx-1]
		if prev.offset+int64(prev.length) == offset && prev.length+len(data) <= len(*prev.bufPtr) {
			bufOff := prev.length
			copy((*prev.bufPtr)[bufOff:bufOff+len(data)], data)
			prev.length += len(data)
			lb.curBytes += int64(len(data))
			atomic.StoreUint64(&prev.seq, seq)
			if pinForWrite {
				atomic.AddInt32(&prev.refCount, 1)
			}
			lb.evictLocked()
			return writeReq{
				offset:    offset,
				bufOffset: bufOff,
				length:    len(data),
				bufPtr:    prev.bufPtr,
				owner:     prev,
			}
		}
	}

	refCount := int32(1)
	if pinForWrite {
		refCount = 2
	}
	chunk := newPooledChunk(offset, data, refCount)
	atomic.StoreUint64(&chunk.seq, seq)

	if idx < len(lb.chunks) && lb.chunks[idx].offset == offset {
		old := lb.chunks[idx]
		lb.curBytes -= int64(old.length)
		old.release()
		lb.chunks[idx] = chunk
	} else {
		lb.chunks = append(lb.chunks, nil)
		copy(lb.chunks[idx+1:], lb.chunks[idx:])
		lb.chunks[idx] = chunk
	}
	lb.curBytes += int64(chunk.length)

	lb.evictLocked()

	return writeReq{
		offset:    offset,
		bufOffset: 0,
		length:    len(data),
		bufPtr:    chunk.bufPtr,
		owner:     chunk,
	}
}

func (lb *LiveBuffer) evictLocked() {
	// Evict least recently used/added chunks if exceeding maxBytes (or max backing buffers),
	// but never evict a chunk that is still in-flight to disk (refCount > 1).
	maxBackingChunks := int(lb.maxBytes / (DefaultWritebackChunkSize / 2))
	for len(lb.chunks) > 0 && (lb.curBytes > lb.maxBytes || (maxBackingChunks >= 2 && len(lb.chunks) > maxBackingChunks)) {
		minIdx := -1
		var minSeq uint64
		for i := 0; i < len(lb.chunks); i++ {
			if atomic.LoadInt32(&lb.chunks[i].refCount) > 1 {
				continue
			}
			s := atomic.LoadUint64(&lb.chunks[i].seq)
			if minIdx == -1 || s < minSeq {
				minSeq = s
				minIdx = i
			}
		}
		if minIdx == -1 {
			// All chunks in LiveBuffer are still in-flight to disk; keep them pinned in RAM until written.
			break
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

// FlushToFile writes all currently retained chunks in LiveBuffer to file
func (lb *LiveBuffer) FlushToFile(file *os.File) error {
	if lb == nil || file == nil {
		return nil
	}

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	for _, c := range lb.chunks {
		if c != nil && c.bufPtr != nil && c.length > 0 {
			_, err := file.WriteAt((*c.bufPtr)[:c.length], c.offset)
			if err != nil {
				return err
			}
		}
	}
	return nil
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

type writeReq struct {
	offset    int64
	bufOffset int
	length    int
	bufPtr    *[]byte
	owner     *pooledChunk
}

// WritebackPipeline coordinates decoupled disk writes and continuous writeback pacing
type WritebackPipeline struct {
	fileMu     sync.RWMutex
	file       *os.File
	fd         uintptr
	state      *FileCacheState
	liveBuffer *LiveBuffer
	queue      chan writeReq
	mu         sync.RWMutex
	closed     bool
	wg         sync.WaitGroup
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

	queueCap := int(bufferBytes / 8192)
	if queueCap < 256 {
		queueCap = 256
	}
	if queueCap > 4096 {
		queueCap = 4096
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
		queue:      make(chan writeReq, queueCap),
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

	var req writeReq
	if wp.liveBuffer != nil {
		req = wp.liveBuffer.PutSlice(offset, data, true)
	} else {
		chunk := newPooledChunk(offset, data, 1)
		req = writeReq{
			offset:    offset,
			bufOffset: 0,
			length:    len(data),
			bufPtr:    chunk.bufPtr,
			owner:     chunk,
		}
	}

	// Queue for asynchronous disk writing & kernel writeback
	select {
	case wp.queue <- req:
		wp.mu.RUnlock()
		return nil
	case <-ctx.Done():
		wp.mu.RUnlock()
		if req.owner != nil {
			req.owner.release() // release consumer's reference
		}
		return ctx.Err()
	}
}

// AttachFile binds a newly materialized disk file to the writeback pipeline and flushes in-memory chunks
func (wp *WritebackPipeline) AttachFile(file *os.File) error {
	if wp == nil || file == nil {
		return nil
	}

	wp.fileMu.Lock()
	defer wp.fileMu.Unlock()

	if wp.file != nil {
		return nil
	}

	wp.file = file
	wp.fd = file.Fd()

	if wp.liveBuffer != nil {
		if err := wp.liveBuffer.FlushToFile(file); err != nil {
			log.Printf("[WritebackPipeline] Error flushing LiveBuffer to file: %v", err)
			return err
		}
	}
	return nil
}

// storageConsumer runs as a dedicated consumer goroutine writing chunks to disk
func (wp *WritebackPipeline) storageConsumer() {
	defer wp.wg.Done()

	flushBatch := func(writeOffset int64, bufPtr *[]byte, bufStart, bufEnd int, owners []*pooledChunk) {
		wp.fileMu.RLock()
		file := wp.file
		wp.fileMu.RUnlock()

		length := bufEnd - bufStart
		if file != nil && bufPtr != nil && length > 0 {
			_, err := file.WriteAt((*bufPtr)[bufStart:bufEnd], writeOffset)
			if err != nil {
				log.Printf("[WritebackPipeline] WriteAt error at offset %d: %v", writeOffset, err)
				wp.errMu.Lock()
				if wp.lastErr == nil {
					wp.lastErr = err
				}
				wp.errMu.Unlock()
			}
		}

		// Writeback pacing (Linux SyncFileRange + Fadvise)
		if length > 0 {
			wp.paceWriteback(writeOffset, int64(length))
		}

		// Release chunk references for consumer
		for _, owner := range owners {
			if owner != nil {
				owner.release()
			}
		}
	}

	for req := range wp.queue {
		writeOffset := req.offset
		bufPtr := req.bufPtr
		bufStart := req.bufOffset
		bufEnd := req.bufOffset + req.length
		owners := []*pooledChunk{req.owner}

	coalesceLoop:
		for {
			select {
			case next, ok := <-wp.queue:
				if !ok {
					break coalesceLoop
				}
				if next.bufPtr == bufPtr && next.bufOffset == bufEnd && next.offset == writeOffset+int64(bufEnd-bufStart) {
					bufEnd += next.length
					owners = append(owners, next.owner)
				} else {
					flushBatch(writeOffset, bufPtr, bufStart, bufEnd, owners)
					writeOffset = next.offset
					bufPtr = next.bufPtr
					bufStart = next.bufOffset
					bufEnd = next.bufOffset + next.length
					owners = []*pooledChunk{next.owner}
				}
			default:
				break coalesceLoop
			}
		}

		flushBatch(writeOffset, bufPtr, bufStart, bufEnd, owners)
	}

	// Final flush on consumer exit
	wp.flushFinalWriteback()
}

// paceWriteback accumulates written bytes into windows and paces asynchronous flush to physical disk
func (wp *WritebackPipeline) paceWriteback(offset int64, length int64) {
	wp.fileMu.RLock()
	fd := wp.fd
	wp.fileMu.RUnlock()

	if fd == 0 {
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
				SyncFileRangeWrite(fd, oldest.start, span)
				SyncFileRangeWaitAndEvict(fd, oldest.start, span)
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
		SyncFileRangeWrite(fd, currOffset, currLength)

		// 2. Await hardware writeout completion of previous window and evict clean pages from host RAM
		if wp.prevWindowLength > 0 {
			SyncFileRangeWaitAndEvict(fd, wp.prevWindowOffset, wp.prevWindowLength)
		}

		wp.prevWindowOffset = currOffset
		wp.prevWindowLength = currLength
		w.start = w.end
	}
}

// flushFinalWriteback cleanly flushes and evicts remaining written bytes
func (wp *WritebackPipeline) flushFinalWriteback() {
	wp.fileMu.RLock()
	fd := wp.fd
	wp.fileMu.RUnlock()

	if fd == 0 {
		return
	}

	for _, w := range wp.windows {
		span := w.end - w.start
		if span > 0 {
			SyncFileRangeWrite(fd, w.start, span)
			SyncFileRangeWaitAndEvict(fd, w.start, span)
		}
	}
	wp.windows = nil

	if wp.prevWindowLength > 0 {
		SyncFileRangeWaitAndEvict(fd, wp.prevWindowOffset, wp.prevWindowLength)
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
