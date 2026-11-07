package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"
)

var globalClientIDSeq uint64

type CacheReader struct {
	state             *FileCacheState
	offset            int64
	clientID          string
	ctx               context.Context
	file              *os.File
	fileMu            sync.Mutex
	lastEvictedOffset int64

	s3Reader   StorageReadSeeker
	s3ReaderMu sync.Mutex

	chunkBuf    []byte
	chunkBufIdx int
	chunkBufOff int64
	chunkBufMu  sync.Mutex
}

func NewCacheReader(ctx context.Context, state *FileCacheState, req *http.Request) *CacheReader {
	state.Lock.Lock()

	// Generate a unique client ID using atomic counter
	clientID := fmt.Sprintf("client-%d", atomic.AddUint64(&globalClientIDSeq, 1))
	state.Clients[clientID] = make(chan struct{}, 1)

	var remoteAddr, userAgent, username string
	if req != nil {
		remoteAddr = req.RemoteAddr
		userAgent = req.Header.Get("User-Agent")
		username = req.URL.Query().Get("user")
		if username == "" {
			username = req.URL.Query().Get("username")
		}
	}
	if userAgent == "" {
		userAgent = "Unknown"
	}
	if remoteAddr == "" {
		remoteAddr = "Internal"
	}

	isCompleted := state.RangeSet.IsComplete(state.TotalSize)

	// Copy necessary state fields and release state.Lock before invoking GlobalRegistry.RegisterConnection
	// to avoid a deadlock due to lock inversion with GlobalRegistry.Lock.
	totalSize := state.TotalSize
	key := state.Key
	url := state.URL
	state.Lock.Unlock()

	var startOffset int64
	if req != nil {
		startOffset = parseRangeStart(req.Header.Get("Range"))
	}

	conn := &ClientConnection{
		ID:             clientID,
		Key:            key,
		URL:            url,
		RemoteAddr:     remoteAddr,
		UserAgent:      userAgent,
		Username:       username,
		Offset:         startOffset,
		StartOffset:    startOffset,
		TotalSize:      totalSize,
		StartTime:      time.Now().Unix(),
		InCache:        isCompleted,
		StarvingOffset: -1,
	}
	GlobalRegistry.RegisterConnection(clientID, conn)

	r := &CacheReader{
		state:             state,
		clientID:          clientID,
		ctx:               ctx,
		lastEvictedOffset: startOffset,
	}

	log.Printf("[Reader] Registered client %s (%s, UA: %s) for media %s", clientID, remoteAddr, userAgent, key)
	return r
}

func (r *CacheReader) getFile() (*os.File, error) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if r.file != nil {
		return r.file, nil
	}
	f, err := os.Open(r.state.PartPath)
	if err != nil {
		f, err = os.Open(r.state.Path)
		if err != nil {
			return nil, err
		}
	}
	r.file = f
	return r.file, nil
}

func (r *CacheReader) getS3Reader() (StorageReadSeeker, error) {
	r.s3ReaderMu.Lock()
	defer r.s3ReaderMu.Unlock()
	if r.s3Reader != nil {
		return r.s3Reader, nil
	}
	if GlobalStorage == nil {
		return nil, errors.New("storage backend not initialized")
	}
	s3r, err := GlobalStorage.Open(r.ctx, r.state.Key)
	if err != nil {
		return nil, err
	}
	r.s3Reader = s3r
	return r.s3Reader, nil
}

func (r *CacheReader) Close() error {
	r.fileMu.Lock()
	if r.file != nil {
		FadviseDontNeed(r.file.Fd(), 0, 0)
		_ = r.file.Close()
		r.file = nil
	}
	r.fileMu.Unlock()

	r.s3ReaderMu.Lock()
	if r.s3Reader != nil {
		_ = r.s3Reader.Close()
		r.s3Reader = nil
	}
	r.s3ReaderMu.Unlock()

	r.state.Lock.Lock()

	// Clean up client channels
	if ch, ok := r.state.Clients[r.clientID]; ok {
		close(ch)
		delete(r.state.Clients, r.clientID)
	}
	delete(r.state.Starving, r.clientID)
	delete(r.state.StarvingSince, r.clientID)

	log.Printf("[Reader] Unregistered client %s from media %s", r.clientID, r.state.Key)

	isCompleted := r.state.RangeSet.IsComplete(r.state.TotalSize)
	shouldFinalize := len(r.state.Clients) == 0 && isCompleted && !r.state.WriterActive && !FileExists(r.state.Path)
	isPrehydrating := r.state.IsPrehydrating || r.state.IsYielding
	shouldCleanUpIncomplete := len(r.state.Clients) == 0 && !isCompleted && !r.state.WriterActive && !isPrehydrating && (GlobalConfig != nil && GlobalConfig.GetEvictYieldedParts())

	var fileToClose *os.File
	var pipelineToClose StorageSink
	if len(r.state.Clients) == 0 && !r.state.WriterActive && !shouldFinalize && shouldCleanUpIncomplete {
		fileToClose = r.state.File
		r.state.File = nil
		pipelineToClose = r.state.Pipeline
		r.state.Pipeline = nil
	}
	remainingClients := len(r.state.Clients)
	r.state.Lock.Unlock()

	GlobalRegistry.UnregisterConnection(r.clientID)

	if shouldFinalize {
		r.finalizeCache()
	} else {
		if pipelineToClose != nil {
			_ = pipelineToClose.FlushAndClose()
		}
		if fileToClose != nil {
			FadviseDontNeed(fileToClose.Fd(), 0, 0)
			_ = fileToClose.Close()
		}
		if shouldCleanUpIncomplete {
			_ = os.Remove(r.state.PartPath)
			_ = os.Remove(r.state.MetaPath)
			GlobalRegistry.Lock.Lock()
			if existingState, exists := GlobalRegistry.Files[r.state.Key]; exists && existingState == r.state {
				delete(GlobalRegistry.Files, r.state.Key)
			}
			GlobalRegistry.Lock.Unlock()
			log.Printf("[Reader] Removed incomplete cache file on last client exit: %s", r.state.Key)
		} else if remainingClients == 0 && !isCompleted {
			// Downshift incomplete hydration to prehydration mode
			if GlobalPrehydrationManager != nil {
				GlobalPrehydrationManager.OnDownshift(r.state)
			}
		}
	}

	if remainingClients == 0 {
		debug.FreeOSMemory()
	}

	return nil
}

func (r *CacheReader) finalizeCache() {
	// Close write handle if open
	r.state.Lock.Lock()
	fileToClose := r.state.File
	r.state.File = nil
	pipeline := r.state.Pipeline
	r.state.Pipeline = nil
	if r.state.LiveBuffer != nil {
		r.state.LiveBuffer.Clear()
		r.state.LiveBuffer = nil
	}
	r.state.Lock.Unlock()

	if pipeline != nil {
		_ = pipeline.FlushAndClose()
	}
	if fileToClose != nil {
		FadviseDontNeed(fileToClose.Fd(), 0, 0)
		_ = fileToClose.Close()
	}

	meta := FileMeta{
		URL:           r.state.URL,
		TotalSize:     r.state.TotalSize,
		ContentType:   r.state.ContentType,
		ETag:          r.state.ETag,
		LastModified:  r.state.LastModified,
		LastValidated: time.Now().Unix(),
		Completed:     true,
	}

	if GlobalStorage != nil && GlobalStorage.Type() == "s3" {
		if r.state.ChunkManager != nil {
			r.state.ChunkManager.AttemptRecompose()
		} else {
			_ = GlobalStorage.CommitLocalFile(context.Background(), r.state.Key, r.state.PartPath, meta)
		}
		_ = os.Remove(r.state.PartPath)
		_ = os.Remove(r.state.MetaPath)

		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, r.state.Key)
		GlobalRegistry.Lock.Unlock()

		debug.FreeOSMemory()
		log.Printf("[Reader] Cache file finalized and committed to S3 on last client exit: %s", r.state.Key)
		return
	}

	// Rename temporary file to final cache path
	err := os.Rename(r.state.PartPath, r.state.Path)
	if err != nil {
		log.Printf("[Reader] Error renaming cache file on final client exit: %v", err)
		return
	}

	if cf, ferr := os.Open(r.state.Path); ferr == nil {
		FadviseDontNeed(cf.Fd(), 0, 0)
		_ = cf.Close()
	}

	// Save final metadata JSON confirming completion
	metaPath := r.state.Path + ".json"
	meta = FileMeta{
		URL:           r.state.URL,
		TotalSize:     r.state.TotalSize,
		ETag:          r.state.ETag,
		LastModified:  r.state.LastModified,
		LastValidated: time.Now().Unix(),
		Completed:     true,
	}
	newBytes, err := json.Marshal(meta)
	if err == nil {
		_ = os.WriteFile(metaPath, newBytes, 0644)
	}

	// Remove from active registry
	GlobalRegistry.Lock.Lock()
	delete(GlobalRegistry.Files, r.state.Key)
	GlobalRegistry.Lock.Unlock()

	debug.FreeOSMemory()

	log.Printf("[Reader] Cache file finalized on last client exit: %s", r.state.Key)
}

func (r *CacheReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off >= r.state.TotalSize {
		return 0, io.EOF
	}

	// Ensure we don't read past the file boundary
	if off+int64(len(p)) > r.state.TotalSize {
		p = p[:r.state.TotalSize-off]
	}

	// Wait until at least some bytes are available from `off`
	for {
		avail := r.state.RangeSet.AvailableFrom(off)
		if avail > 0 || r.state.RangeSet.IsComplete(r.state.TotalSize) {
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			delete(r.state.StarvingSince, r.clientID)
			r.state.Lock.Unlock()
			GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
			break
		}

		select {
		case <-r.ctx.Done():
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			delete(r.state.StarvingSince, r.clientID)
			r.state.Lock.Unlock()
			return 0, r.ctx.Err()
		default:
		}

		r.state.Lock.Lock()
		if !r.state.WriterActive {
			r.state.Lock.Unlock()
			log.Printf("[Reader] Upstream coordinator inactive for %s while offset %d is required. Resuscitation triggered.", r.state.Key, off)
			StartCoordinator(r.state, nil)
			r.state.Lock.Lock()
		}

		if _, exists := r.state.StarvingSince[r.clientID]; !exists {
			if r.state.StarvingSince == nil {
				r.state.StarvingSince = make(map[string]time.Time)
			}
			r.state.StarvingSince[r.clientID] = time.Now()
		}
		r.state.Starving[r.clientID] = off
		clientChan := r.state.Clients[r.clientID]
		r.state.Lock.Unlock()

		GlobalRegistry.UpdateConnectionStarvation(r.clientID, off)

		select {
		case r.state.NotifyChan <- struct{}{}:
		default:
		}

		if clientChan == nil {
			return 0, errors.New("client unregistered")
		}

		select {
		case _, ok := <-clientChan:
			if !ok {
				return 0, errors.New("client channel closed")
			}
		case <-r.ctx.Done():
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			delete(r.state.StarvingSince, r.clientID)
			r.state.Lock.Unlock()
			return 0, r.ctx.Err()
		}
	}

	avail := r.state.RangeSet.AvailableFrom(off)
	toRead := int64(len(p))
	if avail > 0 && toRead > avail {
		toRead = avail
	}

	// 1. LiveBuffer Fast-Path: check in-memory buffer first for zero-copy playback
	if r.state.LiveBuffer != nil {
		if memN, ok := r.state.LiveBuffer.ReadAt(p[:toRead], off); ok && memN > 0 {
			if memN < int(toRead) {
				if f, ferr := r.getFile(); ferr == nil {
					diskN, _ := f.ReadAt(p[memN:toRead], off+int64(memN))
					memN += diskN
				}
			}
			GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(memN))
			if r.state.RangeSet.IsComplete(r.state.TotalSize) {
				GlobalRegistry.SetConnectionInCache(r.clientID, true)
			}
			return memN, nil
		}
	}

	// 2. S3 Storage Tier
	if GlobalStorage != nil && GlobalStorage.Type() == "s3" {
		// A. Recomposed Monolithic Object: If all parts have been stitched and sealed, read via s3ReadSeeker
		isRecomposed := false
		if r.state.ChunkManager != nil && r.state.ChunkManager.IsRecomposed() {
			isRecomposed = true
		} else if !FileExists(r.state.PartPath) {
			isRecomposed = true
		}

		if isRecomposed {
			s3Reader, err := r.getS3Reader()
			if err == nil {
				s3N, rerr := s3Reader.ReadAt(p[:toRead], off)
				if s3N > 0 {
					GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(s3N))
					GlobalRegistry.SetConnectionInCache(r.clientID, true)
					return s3N, nil
				}
				if rerr != nil {
					return 0, rerr
				}
			}
		}

		// B. In-flight Chunks: If chunk is in S3 prior to recomposition, read using sliding chunk buffer
		if r.state.ChunkManager != nil {
			chunkSize := r.state.ChunkManager.ChunkSize()
			chunkIdx := int(off / chunkSize)
			if r.state.ChunkManager.IsChunkUploaded(chunkIdx) {
				chunkStart := int64(chunkIdx) * chunkSize
				offsetInChunk := off - chunkStart
				chunkRemaining := chunkSize - offsetInChunk
				readLen := toRead
				if readLen > chunkRemaining {
					readLen = chunkRemaining
				}

				// Check chunk sliding buffer
				r.chunkBufMu.Lock()
				bufEnd := r.chunkBufOff + int64(len(r.chunkBuf))
				if r.chunkBuf != nil && r.chunkBufIdx == chunkIdx && offsetInChunk >= r.chunkBufOff && offsetInChunk < bufEnd {
					relStart := offsetInChunk - r.chunkBufOff
					avail := int64(len(r.chunkBuf)) - relStart
					n := readLen
					if n > avail {
						n = avail
					}
					copy(p[:n], r.chunkBuf[relStart:relStart+n])
					r.chunkBufMu.Unlock()
					GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(n))
					return int(n), nil
				}

				// Buffer miss: fetch sliding read-ahead window (2MB) from chunk
				fetchSize := int64(2 * 1024 * 1024)
				if readLen > fetchSize {
					fetchSize = readLen
				}
				if offsetInChunk+fetchSize > chunkSize {
					fetchSize = chunkSize - offsetInChunk
				}

				s3ChunkReader, err := GlobalStorage.OpenChunkRange(r.ctx, r.state.Key, chunkIdx, offsetInChunk, fetchSize)
				if err == nil {
					buf, rerr := io.ReadAll(s3ChunkReader)
					_ = s3ChunkReader.Close()
					if (rerr == nil || rerr == io.EOF) && len(buf) > 0 {
						r.chunkBuf = buf
						r.chunkBufIdx = chunkIdx
						r.chunkBufOff = offsetInChunk

						n := readLen
						if n > int64(len(buf)) {
							n = int64(len(buf))
						}
						copy(p[:n], buf[:n])
						r.chunkBufMu.Unlock()
						GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(n))
						return int(n), nil
					}
				}
				r.chunkBufMu.Unlock()

				// If chunk read failed (e.g. chunks deleted during recomposition race), transition to monolithic reader
				if r.state.ChunkManager.IsRecomposed() {
					s3Reader, err := r.getS3Reader()
					if err == nil {
						s3N, rerr := s3Reader.ReadAt(p[:toRead], off)
						if s3N > 0 {
							GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(s3N))
							GlobalRegistry.SetConnectionInCache(r.clientID, true)
							return s3N, nil
						}
						if rerr != nil {
							return 0, rerr
						}
					}
				}
			}
		}
	}

	// 3. Fall back to reading from disk
	file, err := r.getFile()
	if err != nil {
		return 0, err
	}

	n, err = file.ReadAt(p[:toRead], off)
	if n > 0 {
		GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(n))

		curReadEnd := off + int64(n)
		if curReadEnd-r.lastEvictedOffset >= 16*1024*1024 {
			evictLen := curReadEnd - r.lastEvictedOffset - 8*1024*1024
			if evictLen > 0 {
				FadviseDontNeed(file.Fd(), r.lastEvictedOffset, evictLen)
				r.lastEvictedOffset += evictLen
			}
		}

		if r.state.RangeSet.IsComplete(r.state.TotalSize) {
			GlobalRegistry.SetConnectionInCache(r.clientID, true)
		}
	}
	return n, err
}

func (r *CacheReader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.offset)
	r.offset += int64(n)
	return n, err
}

func (r *CacheReader) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = r.offset + offset
	case io.SeekEnd:
		next = r.state.TotalSize + offset
	default:
		return 0, errors.New("invalid whence")
	}
	if next < 0 {
		return 0, errors.New("negative position")
	}
	r.offset = next
	r.lastEvictedOffset = next
	return next, nil
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
