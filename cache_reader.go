package main

import (
	"context"
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
	fileFromPart      bool
	fileMu            sync.Mutex
	lastEvictedOffset int64
	s3Reader   StorageReadSeeker
	s3ReaderMu sync.Mutex

	chunkBuf    []byte
	chunkBufIdx int
	chunkBufOff int64
	chunkBufMu  sync.Mutex

	totalBytesRead   int64
	maxReadOffset    int64
	chainTouchedOnce sync.Once
}

func NewCacheReader(ctx context.Context, state *FileCacheState, req *http.Request) *CacheReader {
	var startOffset int64
	if req != nil {
		startOffset = parseRangeStart(req.Header.Get("Range"))
	}

	state.Lock.Lock()

	// Generate a unique client ID using atomic counter
	clientID := fmt.Sprintf("client-%d", atomic.AddUint64(&globalClientIDSeq, 1))
	state.Clients[clientID] = make(chan struct{}, 1)
	if state.ClientOffsets == nil {
		state.ClientOffsets = make(map[string]int64)
	}
	state.ClientOffsets[clientID] = startOffset

	var remoteAddr, userAgent, username string
	if req != nil {
		remoteAddr = getClientAddr(req)
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

	cacheSource := ""
	if isCompleted {
		if state.GetTargetTier() == TierS3 {
			cacheSource = string(TierS3)
		} else {
			cacheSource = string(TierDisk)
		}
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
		CacheSource:    cacheSource,
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
	f, _, err := r.getFileWithOrigin()
	return f, err
}

func (r *CacheReader) getFileWithOrigin() (*os.File, bool, error) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()

	tier := r.state.GetTargetTier()
	canUseFinal := tier == TierDisk && r.state.Path != "" && FileExists(r.state.Path)
	if r.file != nil {
		if r.fileFromPart && canUseFinal {
			if f, err := os.Open(r.state.Path); err == nil {
				_ = r.file.Close()
				r.file = f
				r.fileFromPart = false
				return r.file, false, nil
			}
		}
		return r.file, r.fileFromPart, nil
	}

	if canUseFinal {
		if f, err := os.Open(r.state.Path); err == nil {
			r.file = f
			r.fileFromPart = false
			return r.file, false, nil
		}
	}

	if !r.state.IsDiskMaterialized() && !canUseFinal {
		_ = r.state.MaterializeDiskFile()
	}

	f, err := os.Open(r.state.PartPath)
	if err == nil {
		r.file = f
		r.fileFromPart = true
		return r.file, true, nil
	}

	if tier == TierDisk && r.state.Path != "" {
		f, err = os.Open(r.state.Path)
		if err == nil {
			r.file = f
			r.fileFromPart = false
			return r.file, false, nil
		}
	}
	return nil, false, err
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
	var s3r StorageReadSeeker
	var err error
	if omb, ok := GlobalStorage.(OpenWithMetaBackend); ok && r.state != nil && r.state.TotalSize > 0 {
		s3r, err = omb.OpenWithMeta(r.ctx, r.state.Key, &FileMeta{TotalSize: r.state.TotalSize}, TierS3)
	} else {
		s3r, err = GlobalStorage.Open(r.ctx, r.state.Key)
	}
	if err != nil {
		return nil, err
	}
	r.s3Reader = s3r
	return r.s3Reader, nil
}

func (r *CacheReader) upgradeFromPrehydration() {
	if r.state == nil {
		return
	}
	r.state.Lock.Lock()
	wasPrehydrating := r.state.IsPrehydrating || r.state.IsYielding
	wasProbe := r.state.IsProbe
	if !wasPrehydrating && !wasProbe {
		r.state.Lock.Unlock()
		return
	}
	r.state.IsPrehydrating = false
	r.state.IsYielding = false
	r.state.IsProbe = false
	key := r.state.Key
	isWriterActive := r.state.WriterActive
	r.state.Lock.Unlock()

	if wasProbe && GlobalRegistry != nil {
		GlobalRegistry.UnregisterProbeBuffer(key)
	}

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
			if GlobalRegistry != nil && r.state.TotalSize > 0 && r.state.GetTargetTier() == TierDisk {
				GlobalRegistry.PreEvict(r.state.TotalSize)
			}
			_ = r.state.MaterializeDiskFile()
		}
		r.upgradeFromPrehydration()
		if r.state != nil {
			r.chainTouchedOnce.Do(func() {
				r.state.Lock.Lock()
				fileURL := r.state.URL
				next := append([]string(nil), r.state.NextURLs...)
				r.state.Lock.Unlock()
				if idx := getGlobalCacheIndex(); idx != nil && len(next) > 0 {
					idx.TouchChain(next)
				}
				if GlobalPrehydrationManager != nil {
					GlobalPrehydrationManager.OnPlayerPlayback(fileURL, next)
				}
			})
		}
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
		r.fileFromPart = false
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
	if r.state.ClientOffsets != nil {
		delete(r.state.ClientOffsets, r.clientID)
	}

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
	isDiskMat := r.state.DiskMaterialized || FileExists(r.state.PartPath)
	isProbe := !isCompleted && !isDiskMat && totalRead < minThreshold && maxOffset < minThreshold

	tier := r.state.targetTierLocked()
	cm := r.state.ChunkManager
	alreadyCommitted := (cm != nil && cm.IsRecomposed()) || (tier == TierDisk && r.state.Path != "" && FileExists(r.state.Path))
	pendingRecomposeExit := tier == TierS3 && remainingClients == 0 && isCompleted && !r.state.WriterActive && !r.state.IsCommitting && !alreadyCommitted && r.state.UpstreamStatus == "Pending Recompose" && !IsS3Healthy()
	shouldFinalize := tier != TierTmp && remainingClients == 0 && isCompleted && !r.state.WriterActive && !r.state.IsCommitting && !alreadyCommitted && !pendingRecomposeExit
	shouldCleanUpTmp := tier == TierTmp && remainingClients == 0 && !isProbe && !r.state.IsCommitting
	shouldCleanUpIncomplete := tier != TierTmp && remainingClients == 0 && !isCompleted && !r.state.WriterActive && !r.state.IsCommitting && !isPrehydrating && (!HasHealthyPersistentTier(GlobalConfig, GlobalStorage) || (GlobalConfig != nil && GlobalConfig.GetEvictYieldedParts()))

	if shouldFinalize {
		r.state.IsCommitting = true
		r.state.UpstreamStatus = "Committing"
	}

	var fileToClose *os.File
	var pipelineToClose StorageSink
	var cmToClose *ScratchChunkManager
	var cancelFunc context.CancelFunc

	if isProbe && remainingClients == 0 {
		if GlobalRegistry != nil {
			GlobalRegistry.RecordProbeActivity()
		}
		cancelFunc = r.state.Cancel
		r.state.Cancel = nil
		r.state.WriterActive = false
		r.state.IsProbe = true
		r.state.IsYielding = true
		r.state.IsPrehydrating = true
		r.state.UpstreamStatus = "Probed (Idle)"
		atomic.StoreInt64(&r.state.LastProgressTimeNano, time.Now().UnixNano())

		fileToClose = r.state.File
		r.state.File = nil
		pipelineToClose = r.state.Pipeline
		r.state.Pipeline = nil
	} else if shouldCleanUpTmp {
		if r.state.WriterActive {
			cancelFunc = r.state.Cancel
			r.state.Cancel = nil
		} else {
			fileToClose = r.state.File
			r.state.File = nil
			pipelineToClose = r.state.Pipeline
			r.state.Pipeline = nil
			cmToClose = r.state.ChunkManager
			r.state.ChunkManager = nil
		}
	} else if pendingRecomposeExit || (remainingClients == 0 && !r.state.WriterActive && !shouldFinalize && shouldCleanUpIncomplete) {
		fileToClose = r.state.File
		r.state.File = nil
		pipelineToClose = r.state.Pipeline
		r.state.Pipeline = nil
		cmToClose = r.state.ChunkManager
		r.state.ChunkManager = nil
	}
	r.state.Lock.Unlock()

	GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
	GlobalRegistry.UnregisterConnection(r.clientID)

	if isProbe && remainingClients == 0 {
		log.Printf("[Reader] Client %s closed probe (%d bytes read). Retaining in-memory probe buffer for %s (status: Probed (Idle))", r.clientID, totalRead, r.state.Key)
		if cancelFunc != nil {
			cancelFunc()
		}
		if pipelineToClose != nil {
			_ = pipelineToClose.FlushAndClose()
		}
		if fileToClose != nil {
			_ = fileToClose.Close()
		}
		if GlobalRegistry != nil {
			GlobalRegistry.RegisterProbeBuffer(r.state.Key)
		}
		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}
		return nil
	}

	if shouldFinalize {
		r.finalizeCache()
	} else if shouldCleanUpTmp {
		if cancelFunc != nil {
			cancelFunc()
		} else {
			if pipelineToClose != nil {
				_ = pipelineToClose.FlushAndClose()
			}
			if cmToClose != nil {
				cmToClose.Close()
			}
			if fileToClose != nil {
				FadviseDontNeed(fileToClose.Fd(), 0, 0)
				_ = fileToClose.Close()
			}
			_ = os.Remove(r.state.PartPath)
			if r.state.MetaPath != "" {
				_ = os.Remove(r.state.MetaPath)
			}
			releaseAccountedCacheBytes(r.state)
			if GlobalRegistry != nil {
				GlobalRegistry.Lock.Lock()
				if existingState, exists := GlobalRegistry.Files[r.state.Key]; exists && existingState == r.state {
					delete(GlobalRegistry.Files, r.state.Key)
				}
				GlobalRegistry.Lock.Unlock()
			}
			if GlobalLogBroker != nil {
				go GlobalLogBroker.BroadcastStateUpdate()
			}
			log.Printf("[Reader] Cleaned up ephemeral TierTmp stream on last client exit: %s", r.state.Key)
		}
	} else {
		if pipelineToClose != nil {
			_ = pipelineToClose.FlushAndClose()
		}
		if cmToClose != nil {
			cmToClose.Close()
		}
		if fileToClose != nil {
			FadviseDontNeed(fileToClose.Fd(), 0, 0)
			_ = fileToClose.Close()
		}
		if shouldCleanUpIncomplete {
			_ = os.Remove(r.state.PartPath)
			if r.state.MetaPath != "" {
				_ = os.Remove(r.state.MetaPath)
			}
			releaseAccountedCacheBytes(r.state)
			triggerAsyncTargetCleanup(cmToClose, GlobalStorage, r.state.Key, "[Reader]")
			GlobalRegistry.Lock.Lock()
			if existingState, exists := GlobalRegistry.Files[r.state.Key]; exists && existingState == r.state {
				delete(GlobalRegistry.Files, r.state.Key)
			}
			GlobalRegistry.Lock.Unlock()
			log.Printf("[Reader] Removed incomplete cache file on last client exit: %s", r.state.Key)
		} else if pendingRecomposeExit {
			GlobalRegistry.Lock.Lock()
			if existingState, exists := GlobalRegistry.Files[r.state.Key]; exists && existingState == r.state {
				delete(GlobalRegistry.Files, r.state.Key)
			}
			GlobalRegistry.Lock.Unlock()
			log.Printf("[Reader] Released active pending-recompose state on last client exit (chunks retained in S3): %s", r.state.Key)
		} else if remainingClients == 0 && !isCompleted && !isPrehydrating && tier != TierTmp {
			// Downshift incomplete hydration to prehydration mode
			if GlobalPrehydrationManager != nil {
				GlobalPrehydrationManager.OnDownshift(r.state)
			}
		}
	}

	if remainingClients == 0 {
		debug.FreeOSMemory()
	}

	if idx := getGlobalCacheIndex(); idx != nil && r.state != nil {
		r.state.FlushTransferToIndex(idx)
	}

	return nil
}

func (r *CacheReader) finalizeCache() {
	commitCompletedFile(r.state, nil, nil, false, "[Reader]")
}

func (r *CacheReader) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.state.TotalSize {
		return 0, io.EOF
	}

	// Ensure we don't read past the file boundary
	if off+int64(len(p)) > r.state.TotalSize {
		p = p[:r.state.TotalSize-off]
	}

	for {
		n, retry, err := r.readAtAttempt(p, off)
		if !retry {
			if n > 0 {
				if r.state != nil {
					atomic.AddInt64(&r.state.TotalBytesServed, int64(n))
				}
				r.state.SetClientOffset(r.clientID, off+int64(n))
				if cm := r.state.GetChunkManager(); cm != nil && cm.IsRingBufferMode() {
					cm.SignalBudget()
				}
			}
			return n, err
		}
		if cerr := r.ctx.Err(); cerr != nil {
			return 0, cerr
		}
	}
}

func (r *CacheReader) readAtAttempt(p []byte, off int64) (n int, retry bool, err error) {
	r.state.Lock.Lock()
	_, registered := r.state.Clients[r.clientID]
	r.state.Lock.Unlock()
	if !registered {
		return 0, false, errors.New("client unregistered")
	}

	r.state.SetClientOffset(r.clientID, off)

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

		// Check if off falls within an unreadable dead zone (capped at MaxDeadZoneSkip = 16MB); if so, synthesize zeroes immediately!
		if gapEnd := r.state.CheckStalledGap(off); gapEnd > off && (gapEnd-off) <= MaxDeadZoneSkip {
			toZero := int64(len(p))
			if toZero > (gapEnd - off) {
				toZero = gapEnd - off
			}
			for i := int64(0); i < toZero; i++ {
				p[i] = 0
			}
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			delete(r.state.StarvingSince, r.clientID)
			r.state.Lock.Unlock()
			GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
			return int(toZero), false, nil
		}

		select {
		case <-r.ctx.Done():
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			delete(r.state.StarvingSince, r.clientID)
			r.state.Lock.Unlock()
			GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
			return 0, false, r.ctx.Err()
		default:
		}

		minThreshold := int64(5 * 1024 * 1024)
		if GlobalConfig != nil {
			minThreshold = GlobalConfig.GetMinReadThresholdBytes()
		}

		r.state.Lock.Lock()
		if _, exists := r.state.StarvingSince[r.clientID]; !exists {
			if r.state.StarvingSince == nil {
				r.state.StarvingSince = make(map[string]time.Time)
			}
			r.state.StarvingSince[r.clientID] = time.Now()
		}
		r.state.Starving[r.clientID] = off
		if r.state.ClientOffsets == nil {
			r.state.ClientOffsets = make(map[string]int64)
		}
		r.state.ClientOffsets[r.clientID] = off

		isTailSeek := r.state.TotalSize > 0 && off >= (r.state.TotalSize-2*1024*1024)
		isMidSeek := off >= minThreshold && !isTailSeek
		needsResuscitation := !r.state.WriterActive
		shouldUpgrade := (needsResuscitation || isMidSeek) && (r.state.IsYielding || r.state.IsPrehydrating || r.state.IsProbe)
		cmStarve := r.state.ChunkManager

		if needsResuscitation || shouldUpgrade {
			wasYielding := r.state.IsYielding
			wasPrehydrating := r.state.IsPrehydrating
			wasProbe := r.state.IsProbe
			if shouldUpgrade {
				r.state.IsYielding = false
				r.state.IsPrehydrating = false
				r.state.IsProbe = false
			}
			key := r.state.Key
			totalSize := r.state.TotalSize
			isDiskMat := r.state.DiskMaterialized
			r.state.Lock.Unlock()

			if shouldUpgrade {
				if wasProbe && GlobalRegistry != nil {
					GlobalRegistry.UnregisterProbeBuffer(key)
				}
				if isMidSeek && !isDiskMat {
					if GlobalRegistry != nil && totalSize > 0 && r.state.GetTargetTier() == TierDisk {
						GlobalRegistry.PreEvict(totalSize)
					}
					_ = r.state.MaterializeDiskFile()
				}
				if wasYielding || wasPrehydrating || wasProbe {
					log.Printf("[Server] Priority Upgrade: File %s upgraded from prehydration/yielding to Live Playback!", key)
					if GlobalPrehydrationManager != nil {
						GlobalPrehydrationManager.OnPriorityUpgrade(key)
					}
				}
			}

			if needsResuscitation {
				log.Printf("[Reader] Upstream coordinator inactive for %s while offset %d is required. Resuscitation triggered.", key, off)
				StartCoordinator(r.state, nil)
			} else if shouldUpgrade {
				r.state.SetUpstreamStatus("Downloading")
			}
			r.state.Lock.Lock()
		}

		clientChan := r.state.Clients[r.clientID]
		r.state.Lock.Unlock()

		GlobalRegistry.UpdateConnectionStarvation(r.clientID, off)
		if cmStarve != nil && cmStarve.IsRingBufferMode() {
			cmStarve.SignalBudget()
		}

		select {
		case r.state.NotifyChan <- struct{}{}:
		default:
		}

		if clientChan == nil {
			return 0, false, errors.New("client unregistered")
		}

		select {
		case _, ok := <-clientChan:
			if !ok {
				return 0, false, errors.New("client channel closed")
			}
		case <-r.ctx.Done():
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			delete(r.state.StarvingSince, r.clientID)
			r.state.Lock.Unlock()
			GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
			return 0, false, r.ctx.Err()
		}
	}

	avail := r.state.RangeSet.AvailableFrom(off)
	toRead := int64(len(p))
	if avail > 0 && toRead > avail {
		toRead = avail
	}

	r.state.Lock.Lock()
	lb := r.state.LiveBuffer
	cm := r.state.ChunkManager
	tier := r.state.targetTierLocked()
	r.state.Lock.Unlock()

	// 1. LiveBuffer Fast-Path: check in-memory buffer first for zero-copy playback
	if lb != nil {
		if memN, ok := lb.ReadAt(p[:toRead], off); ok && memN > 0 {
			isS3Chunked := r.state.UsesScratchManager() && cm != nil
			if memN < int(toRead) && !isS3Chunked && (r.state.IsDiskMaterialized() || FileExists(r.state.PartPath) || FileExists(r.state.Path)) {
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
			return memN, false, nil
		}
	}

	// 2. S3 Storage Tier
	if tier == TierS3 {
		// A. Recomposed Monolithic Object: If all parts have been stitched and sealed, read via s3ReadSeeker
		isRecomposed := false
		if cm != nil {
			isRecomposed = cm.IsRecomposed()
		} else if !FileExists(r.state.PartPath) && r.state.RangeSet.IsComplete(r.state.TotalSize) {
			if idx := getGlobalCacheIndex(); idx == nil || !idx.IsPendingRecompose(r.state.Key) {
				isRecomposed = true
			}
		}

		if isRecomposed {
			s3Reader, err := r.getS3Reader()
			if err == nil {
				s3N, rerr := s3Reader.ReadAt(p[:toRead], off)
				if s3N > 0 {
					GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(s3N))
					GlobalRegistry.SetConnectionInCache(r.clientID, true)
					return s3N, false, nil
				}
				if rerr != nil {
					return 0, false, rerr
				}
			}
		}

		// B. In-flight Chunks: If chunk is in S3 prior to recomposition, read using sliding chunk buffer
		if cm != nil && cm.ChunkSize() > 0 {
			chunkSize := cm.ChunkSize()
			chunkIdx := int(off / chunkSize)
			if cm.IsChunkUploaded(chunkIdx) {
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
					return int(n), false, nil
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
						return int(n), false, nil
					}
					if rerr != nil {
						err = rerr
					}
				}
				r.chunkBufMu.Unlock()

				// Chunk was already uploaded to S3 and hole-punched locally: NEVER fall back to reading zeroes from .part!
				// If OpenChunkRange failed (e.g. DeleteObjects ran milliseconds before m.recomposed=true),
				// immediately try opening the sealed monolithic S3 object.
				s3Reader, s3Err := r.getS3Reader()
				if s3Err == nil {
					s3N, rerr := s3Reader.ReadAt(p[:toRead], off)
					if s3N > 0 {
						GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(s3N))
						GlobalRegistry.SetConnectionInCache(r.clientID, true)
						return s3N, false, nil
					}
					if rerr != nil {
						s3Err = rerr
					}
				}
				if r.ctx.Err() == nil {
					RecordS3Failure(err)
					RecordS3Failure(s3Err)
					cm.InvalidateUploadedChunk(chunkIdx)
					return 0, true, nil
				}
				if err != nil {
					return 0, false, err
				}
				return 0, false, s3Err
			}
		}
	}

	// 2b. Cross-FS NAS Staged Chunks: read committed chunks from CACHE_DIR/<key>.staging before reading finalPath
	if tier == TierDisk && cm != nil {
		alreadyFinal := r.state.Path != "" && FileExists(r.state.Path)
		if !cm.IsRecomposed() && !alreadyFinal {
			chunkSize := cm.ChunkSize()
			if chunkSize > 0 {
				chunkIdx := int(off / chunkSize)
				if cm.IsChunkUploaded(chunkIdx) {
					chunkStart := int64(chunkIdx) * chunkSize
					offsetInChunk := off - chunkStart
					readLen := toRead
					if rem := chunkSize - offsetInChunk; readLen > rem {
						readLen = rem
					}
					rc, cerr := cm.ReadCommittedChunk(r.ctx, chunkIdx, offsetInChunk, readLen)
					if cerr == nil {
						readN, rerr := io.ReadFull(rc, p[:readLen])
						_ = rc.Close()
						if (rerr == nil || rerr == io.EOF || rerr == io.ErrUnexpectedEOF) && readN > 0 {
							r.recordRead(off, readN)
							GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(readN))
							if r.state.RangeSet.IsComplete(r.state.TotalSize) {
								GlobalRegistry.SetConnectionInCache(r.clientID, true)
							}
							return readN, false, nil
						}
						if !(r.state.Path != "" && FileExists(r.state.Path)) {
							if r.ctx.Err() == nil {
								RecordNASFailure(rerr)
								cm.InvalidateUploadedChunk(chunkIdx)
								return 0, true, nil
							}
							if rerr != nil {
								return 0, false, rerr
							}
							return 0, false, io.EOF
						}
					} else if !(r.state.Path != "" && FileExists(r.state.Path)) {
						// Never fall back to hole-punched PartPath when chunk is committed to NAS staging
						if r.ctx.Err() == nil {
							RecordNASFailure(cerr)
							cm.InvalidateUploadedChunk(chunkIdx)
							return 0, true, nil
						}
						return 0, false, cerr
					}
				}
			}
		}
	}

	// 3. Fall back to reading from disk
	file, fromPart, err := r.getFileWithOrigin()
	if err != nil {
		return 0, false, err
	}

	var needRingRetry bool
	var needFlushRetry bool
	var startChunk int
	var anyUploaded bool
	if fromPart && r.state != nil {
		r.state.partIOMu.RLock()
	}
	n, err = file.ReadAt(p[:toRead], off)
	if cm != nil && fromPart && cm.IsRingBufferMode() && n > 0 && !r.state.RangeSet.HasAvailable(off, int64(n)) {
		needRingRetry = true
	}
	if !needRingRetry && cm != nil && fromPart && cm.ChunkSize() > 0 {
		chunkSize := cm.ChunkSize()
		startChunk = int(off / chunkSize)
		endChunk := startChunk
		if n > 0 {
			endChunk = int((off + int64(n) - 1) / chunkSize)
		}
		for c := startChunk; c <= endChunk; c++ {
			if cm.IsChunkUploaded(c) {
				anyUploaded = true
				break
			}
		}
	}
	if !needRingRetry && !anyUploaded && fromPart && n > 0 && r.state != nil && r.state.DiskRangeSet != nil {
		diskTracked := r.state.DiskRangeSet.TotalBytes() > 0 || (cm != nil && cm.IsRingBufferMode())
		if diskTracked && !r.state.DiskRangeSet.HasAvailable(off, int64(n)) {
			needFlushRetry = true
		}
	}
	if fromPart && r.state != nil {
		r.state.partIOMu.RUnlock()
	}
	if needRingRetry {
		return 0, true, nil
	}
	if needFlushRetry {
		select {
		case <-time.After(1 * time.Millisecond):
			return 0, true, nil
		case <-r.ctx.Done():
			return 0, false, r.ctx.Err()
		}
	}
	if cm != nil && fromPart && cm.ChunkSize() > 0 && anyUploaded {
		chunkSize := cm.ChunkSize()
		chunkStart := int64(startChunk) * chunkSize
		offsetInChunk := off - chunkStart
		readLen := toRead
		if rem := chunkSize - offsetInChunk; readLen > rem {
			readLen = rem
		}
		if rc, cerr := cm.ReadCommittedChunk(r.ctx, startChunk, offsetInChunk, readLen); cerr == nil {
			readN, rerr := io.ReadFull(rc, p[:readLen])
			_ = rc.Close()
			if (rerr == nil || rerr == io.EOF || rerr == io.ErrUnexpectedEOF) && readN > 0 {
				n, err = readN, nil
			} else if rerr != nil {
				if r.ctx.Err() == nil {
					if tier == TierS3 {
						RecordS3Failure(rerr)
					} else {
						RecordNASFailure(rerr)
					}
					cm.InvalidateUploadedChunk(startChunk)
					return 0, true, nil
				}
				n, err = 0, rerr
			}
		} else if r.state.GetTargetTier() == TierS3 {
			if s3Reader, s3Err := r.getS3Reader(); s3Err == nil {
				n, err = s3Reader.ReadAt(p[:readLen], off)
			} else if r.ctx.Err() == nil {
				RecordS3Failure(cerr)
				RecordS3Failure(s3Err)
				cm.InvalidateUploadedChunk(startChunk)
				return 0, true, nil
			} else {
				n, err = 0, cerr
			}
		} else if f2, stillFromPart, fErr := r.getFileWithOrigin(); fErr == nil && !stillFromPart {
			n, err = f2.ReadAt(p[:readLen], off)
		} else if r.ctx.Err() == nil {
			RecordNASFailure(cerr)
			cm.InvalidateUploadedChunk(startChunk)
			return 0, true, nil
		} else {
			n, err = 0, cerr
		}
	}
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
	return n, false, err
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
