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
	totalBytesRead    int64
	maxReadOffset     int64
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

	if !isCompleted && !state.IsDiskMaterialized() && GlobalRegistry != nil {
		GlobalRegistry.RecordProbeActivity()
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
	if !r.state.IsDiskMaterialized() && !FileExists(r.state.Path) {
		_ = r.state.MaterializeDiskFile()
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

func (r *CacheReader) upgradeFromPrehydration() {
	if r.state == nil {
		return
	}
	r.state.Lock.Lock()
	wasPrehydrating := r.state.IsPrehydrating || r.state.IsYielding
	if !wasPrehydrating {
		r.state.Lock.Unlock()
		return
	}
	r.state.IsPrehydrating = false
	r.state.IsYielding = false
	key := r.state.Key
	isWriterActive := r.state.WriterActive
	r.state.Lock.Unlock()

	log.Printf("[Server] Priority Upgrade: File %s upgraded from prehydration/yielding to Live Playback!", key)
	if GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.OnPriorityUpgrade(key)
	}
	r.state.SetUpstreamStatus("Connecting")
	if !isWriterActive && !r.state.RangeSet.IsComplete(r.state.TotalSize) {
		StartCoordinator(r.state, nil)
	}
}

func (r *CacheReader) recordRead(off int64, n int) {
	if n <= 0 {
		return
	}
	atomic.AddInt64(&r.totalBytesRead, int64(n))
	newEnd := off + int64(n)
	for {
		oldMax := atomic.LoadInt64(&r.maxReadOffset)
		if newEnd <= oldMax || atomic.CompareAndSwapInt64(&r.maxReadOffset, oldMax, newEnd) {
			break
		}
	}

	minThreshold := int64(5 * 1024 * 1024)
	if GlobalConfig != nil {
		minThreshold = GlobalConfig.GetMinReadThresholdBytes()
	}

	totalRead := atomic.LoadInt64(&r.totalBytesRead)
	isTailSeek := r.state != nil && r.state.TotalSize > 0 && off >= (r.state.TotalSize-2*1024*1024)
	isWholeFile := r.state != nil && r.state.TotalSize > 0 && totalRead >= r.state.TotalSize
	if totalRead >= minThreshold || (off >= minThreshold && !isTailSeek) || isWholeFile {
		if r.state != nil && !r.state.IsDiskMaterialized() {
			if GlobalRegistry != nil && r.state.TotalSize > 0 {
				GlobalRegistry.PreEvict(r.state.TotalSize)
			}
			_ = r.state.MaterializeDiskFile()
		}
		r.upgradeFromPrehydration()
	} else if r.state != nil && !r.state.IsDiskMaterialized() && GlobalRegistry != nil {
		GlobalRegistry.RecordProbeActivity()
	}
}

func (r *CacheReader) Close() error {
	r.fileMu.Lock()
	if r.file != nil {
		FadviseDontNeed(r.file.Fd(), 0, 0)
		_ = r.file.Close()
		r.file = nil
	}
	r.fileMu.Unlock()

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
	remainingClients := len(r.state.Clients)

	minThreshold := int64(5 * 1024 * 1024)
	if GlobalConfig != nil {
		minThreshold = GlobalConfig.GetMinReadThresholdBytes()
	}
	totalRead := atomic.LoadInt64(&r.totalBytesRead)
	maxOffset := atomic.LoadInt64(&r.maxReadOffset)
	isPrehydrating := r.state.IsPrehydrating || r.state.IsYielding
	isProbe := !isCompleted && !r.state.DiskMaterialized && !isPrehydrating && totalRead < minThreshold && maxOffset < minThreshold

	shouldFinalize := remainingClients == 0 && isCompleted && !r.state.WriterActive && !FileExists(r.state.Path)
	shouldCleanUpIncomplete := remainingClients == 0 && !isCompleted && !r.state.WriterActive && !isPrehydrating && (GlobalConfig != nil && GlobalConfig.GetEvictYieldedParts())

	var fileToClose *os.File
	var pipelineToClose StorageSink
	var cancelFunc context.CancelFunc

	if isProbe && remainingClients == 0 {
		if GlobalRegistry != nil {
			GlobalRegistry.RecordProbeActivity()
		}
		cancelFunc = r.state.Cancel
		r.state.Cancel = nil
		r.state.WriterActive = false
		r.state.NextURLs = nil
		fileToClose = r.state.File
		r.state.File = nil
		pipelineToClose = r.state.Pipeline
		r.state.Pipeline = nil
		if r.state.LiveBuffer != nil {
			r.state.LiveBuffer.Clear()
			r.state.LiveBuffer = nil
		}
	} else if remainingClients == 0 && !r.state.WriterActive && !shouldFinalize && shouldCleanUpIncomplete {
		fileToClose = r.state.File
		r.state.File = nil
		pipelineToClose = r.state.Pipeline
		r.state.Pipeline = nil
	}
	r.state.Lock.Unlock()

	GlobalRegistry.UnregisterConnection(r.clientID)

	if isProbe && remainingClients == 0 {
		log.Printf("[Reader] Client %s closed probe (%d bytes read, below %d threshold). Aborted without disk write or downshift for %s", r.clientID, totalRead, minThreshold, r.state.Key)
		if cancelFunc != nil {
			cancelFunc()
		}
		if pipelineToClose != nil {
			_ = pipelineToClose.FlushAndClose()
		}
		if fileToClose != nil {
			_ = fileToClose.Close()
		}
		_ = os.Remove(r.state.PartPath)
		_ = os.Remove(r.state.MetaPath)

		accounted := atomic.SwapInt64(&r.state.TotalBytesDownloaded, 0)
		if accounted > 0 && GlobalRegistry != nil {
			GlobalRegistry.UpdateSize(-accounted)
		}

		GlobalRegistry.Lock.Lock()
		if existingState, exists := GlobalRegistry.Files[r.state.Key]; exists && existingState == r.state {
			delete(GlobalRegistry.Files, r.state.Key)
		}
		GlobalRegistry.Lock.Unlock()

		debug.FreeOSMemory()
		return nil
	}

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
			accounted := atomic.SwapInt64(&r.state.TotalBytesDownloaded, 0)
			if accounted > 0 && GlobalRegistry != nil {
				GlobalRegistry.UpdateSize(-accounted)
			}
			GlobalRegistry.Lock.Lock()
			if existingState, exists := GlobalRegistry.Files[r.state.Key]; exists && existingState == r.state {
				delete(GlobalRegistry.Files, r.state.Key)
			}
			GlobalRegistry.Lock.Unlock()
			log.Printf("[Reader] Removed incomplete cache file on last client exit: %s", r.state.Key)
		} else if remainingClients == 0 && !isCompleted && !isPrehydrating {
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
	meta := FileMeta{
		URL:           r.state.URL,
		TotalSize:     r.state.TotalSize,
		ContentType:   r.state.ContentType,
		ETag:          r.state.ETag,
		LastModified:  r.state.LastModified,
		LastValidated: time.Now().Unix(),
		Completed:     true,
	}
	newBytes, err := json.Marshal(meta)
	if err == nil {
		_ = writeMetaFileAtomic(metaPath, newBytes)
	}

	GlobalRegistry.MarkConnectionsAsInCache(r.state.Key)
	r.state.SetUpstreamStatus("Completed")

	// Remove from active registry
	GlobalRegistry.Lock.Lock()
	delete(GlobalRegistry.Files, r.state.Key)
	GlobalRegistry.Lock.Unlock()

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
	if GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.OnHydrationCompleted(r.state)
	}

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
			wasYielding := r.state.IsYielding
			if wasYielding {
				r.state.IsYielding = false
			}
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
			if memN < int(toRead) && (r.state.IsDiskMaterialized() || FileExists(r.state.PartPath) || FileExists(r.state.Path)) {
				if f, ferr := r.getFile(); ferr == nil {
					diskN, _ := f.ReadAt(p[memN:toRead], off+int64(memN))
					memN += diskN
				}
			}
			r.recordRead(off, memN)
			GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(memN))
			if r.state.RangeSet.IsComplete(r.state.TotalSize) {
				GlobalRegistry.SetConnectionInCache(r.clientID, true)
			}
			return memN, nil
		}
	}

	// 2. Fall back to reading from disk
	file, err := r.getFile()
	if err != nil {
		return 0, err
	}

	n, err = file.ReadAt(p[:toRead], off)
	if n > 0 {
		r.recordRead(off, n)
		GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(n))

		curReadEnd := off + int64(n)
		lastEvict := atomic.LoadInt64(&r.lastEvictedOffset)
		if curReadEnd-lastEvict >= 16*1024*1024 {
			evictLen := curReadEnd - lastEvict - 8*1024*1024
			if evictLen > 0 {
				FadviseDontNeed(file.Fd(), lastEvict, evictLen)
				atomic.AddInt64(&r.lastEvictedOffset, evictLen)
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
	atomic.StoreInt64(&r.lastEvictedOffset, next)
	return next, nil
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
