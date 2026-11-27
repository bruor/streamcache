package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var (
	coordinatorSleepMu sync.RWMutex
	coordinatorSleepFn func(time.Duration)

	probeNextReadableOffsetFn = probeNextReadableOffset
)

// probeNextReadableOffset probes forward exponentially from stallOffset (+256KB, +1MB, +2MB, +4MB, +8MB, +16MB)
// with a fast 2-second timeout per hop to find the next readable byte offset.
func probeNextReadableOffset(ctx context.Context, targetURL string, stallOffset int64, totalSize int64, userAgent string) (int64, error) {
	if stallOffset < 0 {
		stallOffset = 0
	}
	hops := []int64{
		256 * 1024,
		1 * 1024 * 1024,
		2 * 1024 * 1024,
		4 * 1024 * 1024,
		8 * 1024 * 1024,
		16 * 1024 * 1024,
	}

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	for _, hop := range hops {
		if ctx != nil && ctx.Err() != nil {
			return 0, ctx.Err()
		}

		probeStart := stallOffset + hop
		if totalSize > 0 && probeStart >= totalSize {
			break
		}

		probeEnd := probeStart + 1024 - 1
		if totalSize > 0 && probeEnd >= totalSize {
			probeEnd = totalSize - 1
		}

		req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
		if err != nil {
			continue
		}

		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", probeStart, probeEnd))
		if userAgent != "" {
			req.Header.Set("User-Agent", userAgent)
		} else {
			req.Header.Set("User-Agent", "Streamcache/1.0")
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[DeadZoneProbe] Probe at offset %d failed: %v", probeStart, err)
			continue
		}

		if resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK {
			buf := make([]byte, 1)
			_, rerr := io.ReadFull(resp.Body, buf)
			resp.Body.Close()
			if rerr == nil {
				log.Printf("[DeadZoneProbe] Found readable byte at offset %d (+%d KB from stalled %d)",
					probeStart, hop/1024, stallOffset)
				return probeStart, nil
			}
		} else {
			resp.Body.Close()
			log.Printf("[DeadZoneProbe] Probe at offset %d returned HTTP %d", probeStart, resp.StatusCode)
		}
	}

	return 0, fmt.Errorf("no readable offset found within 16MB probe window")
}

// StartCoordinator spawns the manager goroutine for a given file
func StartCoordinator(state *FileCacheState, initialHeader http.Header) {
	state.Lock.Lock()
	if state.WriterActive {
		state.Lock.Unlock()
		return
	}
	state.WriterActive = true
	state.coordGen++
	myGen := state.coordGen
	cm := state.ChunkManager
	state.Lock.Unlock()

	if cm != nil {
		cm.ResumeUploads()
	}

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}

	go runCoordinator(state, initialHeader, myGen)
}

// CancelCoordinator cancels an active coordinator, stopping workers and deleting partial files if no clients exist
func CancelCoordinator(state *FileCacheState) {
	state.Lock.Lock()
	cancel := state.Cancel
	state.Cancel = nil
	state.UpstreamStatus = "Cancelled"
	state.IsPrehydrating = false
	state.IsYielding = false
	state.IsProbe = false
	state.coordGen++
	clientsCount := len(state.Clients)
	for id, ch := range state.Clients {
		close(ch)
		delete(state.Clients, id)
	}
	state.Starving = make(map[string]int64)
	state.StarvingSince = make(map[string]time.Time)
	if state.ClientOffsets != nil {
		state.ClientOffsets = make(map[string]int64)
	}
	fileToClose := state.File
	state.File = nil
	pipelineToClose := state.Pipeline
	state.Pipeline = nil
	state.WriterActive = false
	partPath := state.PartPath
	metaPath := state.MetaPath
	key := state.Key
	if state.LiveBuffer != nil {
		state.LiveBuffer.Clear()
		state.LiveBuffer = nil
	}
	chunkManager := state.ChunkManager
	state.ChunkManager = nil
	state.Lock.Unlock()

	if GlobalRegistry != nil {
		GlobalRegistry.UnregisterProbeBuffer(key)
	}

	if GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.OnCancelled(key)
	}

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}

	if cancel != nil {
		cancel()
	}

	if pipelineToClose != nil {
		_ = pipelineToClose.FlushAndClose()
	}

	if chunkManager != nil {
		chunkManager.Close()
	}

	if fileToClose != nil {
		FadviseDontNeed(fileToClose.Fd(), 0, 0)
		_ = fileToClose.Close()
	}

	if clientsCount == 0 {
		GlobalRegistry.Lock.Lock()
		existingState, exists := GlobalRegistry.Files[key]
		if exists && (existingState == state || existingState == nil) {
			delete(GlobalRegistry.Files, key)
			GlobalRegistry.Lock.Unlock()
			releaseAccountedCacheBytes(state)
			if FileExists(partPath) {
				_ = os.Remove(partPath)
			}
			if metaPath != "" && FileExists(metaPath) {
				_ = os.Remove(metaPath)
			}
			if chunkManager == nil || !chunkManager.IsRecomposed() {
				triggerAsyncTargetCleanup(chunkManager, GlobalStorage, key, "[Coord]")
			}
			log.Printf("[Coord] Manually cancelled hydration and removed partial cache files: %s", key)
		} else {
			GlobalRegistry.Lock.Unlock()
		}
	} else {
		log.Printf("[Coord] Manually cancelled hydration for %s (%d active clients remain)", key, clientsCount)
	}

	GlobalRegistry.ClearFailedProbe(key)

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
	}
	runtime.GC()
	debug.FreeOSMemory()
}

type Worker struct {
	id                int
	startOffset       int64
	cancel            context.CancelFunc
	currentOffset     int64 // guarded atomically
	targetEndOffset   int64 // guarded atomically
	speed             int64 // estimated speed (bytes/sec)
	spawnTimeNano     int64 // Unix nano timestamp of worker spawn
	connStartTimeNano int64 // Unix nano timestamp of connection start, guarded atomically
	connBytesWritten  int64 // bytes read on this connection, guarded atomically
	lastByteTimeNano  int64 // Unix nano timestamp of last byte received from upstream, guarded atomically
	isPrefetching     int32 // 1 if prefetching tail metadata, 0 otherwise, guarded atomically
	isPrefetchHelper  bool
}

func runCoordinator(state *FileCacheState, initialHeader http.Header, myGen uint64) {
	state.coordMu.Lock()
	defer state.coordMu.Unlock()

	state.Lock.Lock()
	if state.coordGen != myGen {
		state.Lock.Unlock()
		return
	}
	shouldMaterializeImmediately := state.IsPrehydrating || FileExists(state.PartPath)
	state.Lock.Unlock()

	log.Printf("[Coord] Starting concurrent background coordinator for cache: %s", state.Key)

	if shouldMaterializeImmediately {
		if err := state.MaterializeDiskFile(); err != nil {
			log.Printf("[Coord] Error materializing part file: %v", err)
			state.Lock.Lock()
			if state.coordGen == myGen {
				state.WriterActive = false
			}
			state.Lock.Unlock()
			return
		}
	}

	var file *os.File
	state.Lock.Lock()
	file = state.File
	pipeline := state.Pipeline
	state.Lock.Unlock()

	if pipeline == nil {
		pipeline = NewWritebackPipeline(file, state, GlobalConfig.GetWritebackBufferSize())
		state.Lock.Lock()
		state.Pipeline = pipeline
		state.Lock.Unlock()
	}

	state.Lock.Lock()
	if state.HydrationStartTime == 0 {
		state.HydrationStartTime = time.Now().Unix()
	}
	state.Lock.Unlock()

	parentCtx, parentCancel := context.WithCancel(context.Background())
	state.Lock.Lock()
	state.Cancel = parentCancel
	state.Lock.Unlock()

	workerIdGen := 0
	var workersWg sync.WaitGroup

	defer func() {
		parentCancel()
		workersWg.Wait()
		state.Lock.Lock()
		isCurrentGen := state.coordGen == myGen
		if isCurrentGen {
			state.Cancel = nil
		}

		// Wake up any client readers waiting on data
		for _, ch := range state.Clients {
			safeSignal(ch)
		}

		isCompleted := state.RangeSet.IsComplete(state.TotalSize)

		if isCompleted {
			shouldMaterialize := !state.DiskMaterialized
			state.Lock.Unlock()

			if shouldMaterialize {
				_ = state.MaterializeDiskFile()
			}

			state.Lock.Lock()
			fileToClose := state.File
			state.File = nil
			pipelineToClose := state.Pipeline
			state.Pipeline = nil
			state.IsCommitting = true
			state.UpstreamStatus = "Committing"
			state.Lock.Unlock()

			if GlobalLogBroker != nil {
				go GlobalLogBroker.BroadcastStateUpdate()
			}
			log.Printf("[Coord] Committing completed cache file to disk: %s", state.Key)

			go finalizeAndCommitCache(state, fileToClose, pipelineToClose)
		} else {
			var fileToClose *os.File
			var cmToClose *ScratchChunkManager
			pipelineToClose := state.Pipeline
			state.Pipeline = nil
			tier := state.targetTierLocked()
			pm := GlobalPrehydrationManager
			canSuspendToYield := tier != TierTmp && state.DiskMaterialized && state.UpstreamStatus != "Cancelled" && pm != nil && (GlobalConfig == nil || (GlobalConfig.GetEnableEpisodePrehydration() && !GlobalConfig.GetEvictYieldedParts()))
			shouldSuspendToYield := isCurrentGen && len(state.Clients) == 0 && !state.IsYielding && (state.IsPrehydrating || canSuspendToYield)
			shouldCleanUpIncomplete := isCurrentGen && len(state.Clients) == 0 && !state.IsYielding && (!state.IsPrehydrating || tier == TierTmp) && !shouldSuspendToYield
			if shouldCleanUpIncomplete {
				fileToClose = state.File
				state.File = nil
				cmToClose = state.ChunkManager
				state.ChunkManager = nil
			}
			if isCurrentGen {
				state.WriterActive = false
				// Reset Sniffed on incomplete exit so a restarted coordinator
				// re-sniffs and schedules uncached tail probes.
				state.Sniffed = false
			}
			state.Lock.Unlock()

			if pipelineToClose != nil {
				_ = pipelineToClose.FlushAndClose()
			}
			if cmToClose != nil {
				cmToClose.Close()
			}
			if fileToClose != nil {
				_ = fileToClose.Close()
			}

			if shouldSuspendToYield && pm != nil {
				pm.YieldState(state, "Coordinator exited before completion; suspended for retry")
			} else if shouldCleanUpIncomplete {
				shouldDeleteReg := false
				GlobalRegistry.Lock.RLock()
				if existingState, exists := GlobalRegistry.Files[state.Key]; exists && (existingState == state || existingState == nil) {
					shouldDeleteReg = true
				}
				GlobalRegistry.Lock.RUnlock()

				if shouldDeleteReg {
					releaseAccountedCacheBytes(state)
					_ = os.Remove(state.PartPath)
					if state.MetaPath != "" {
						_ = os.Remove(state.MetaPath)
					}
					triggerAsyncTargetCleanup(cmToClose, GlobalStorage, state.Key, "[Coord]")

					GlobalRegistry.Lock.Lock()
					if cur, exists := GlobalRegistry.Files[state.Key]; exists && (cur == state || cur == nil) {
						delete(GlobalRegistry.Files, state.Key)
					}
					GlobalRegistry.Lock.Unlock()
					log.Printf("[Coord] Removed incomplete cache file from registry and disk: %s", state.Key)
				}
			}
		}

		log.Printf("[Coord] Exiting background coordinator for cache: %s", state.Key)
	}()

	removeWorker := func(w *Worker) {
		state.WorkersLock.Lock()
		defer state.WorkersLock.Unlock()
		for i, aw := range state.ActiveWorkers {
			if aw.id == w.id {
				state.ActiveWorkers = append(state.ActiveWorkers[:i], state.ActiveWorkers[i+1:]...)
				break
			}
		}
		select {
		case state.NotifyChan <- struct{}{}:
		default:
		}
	}

	spawnWorkerAt := func(startOffset int64, allowPrehydrateMulti bool) {
		state.WorkersLock.Lock()
		defer state.WorkersLock.Unlock()

		state.Lock.Lock()
		isPrehydrating := state.IsPrehydrating
		hasStarving := false
		for _, off := range state.Starving {
			if !state.RangeSet.Contains(off) {
				hasStarving = true
				break
			}
		}
		state.Lock.Unlock()

		if !allowPrehydrateMulti && isPrehydrating && !hasStarving && len(state.ActiveWorkers) >= 1 {
			return
		}

		workerIdGen++
		wCtx, wCancel := context.WithCancel(parentCtx)
		w := &Worker{
			id:               workerIdGen,
			startOffset:      startOffset,
			cancel:           wCancel,
			currentOffset:    startOffset,
			targetEndOffset:  state.TotalSize,
			spawnTimeNano:    time.Now().UnixNano(),
			lastByteTimeNano: time.Now().UnixNano(),
		}

		// Find next downstream boundary (next cached chunk or downstream worker)
		gapStart, gapEnd, found := state.RangeSet.FindNextGap(startOffset, state.TotalSize)
		if found && gapStart == startOffset && gapEnd > startOffset {
			w.targetEndOffset = gapEnd
		}
		for _, other := range state.ActiveWorkers {
			otherCur := atomic.LoadInt64(&other.currentOffset)
			if otherCur > startOffset && otherCur < w.targetEndOffset {
				w.targetEndOffset = otherCur
			}
			if other.startOffset > startOffset && other.startOffset < w.targetEndOffset {
				w.targetEndOffset = other.startOffset
			}
		}

		// Immediately clamp any trailing active workers running behind startOffset
		for _, other := range state.ActiveWorkers {
			otherCur := atomic.LoadInt64(&other.currentOffset)
			if otherCur < startOffset {
				otherTarget := atomic.LoadInt64(&other.targetEndOffset)
				if otherTarget <= 0 || otherTarget > startOffset {
					atomic.StoreInt64(&other.targetEndOffset, startOffset)
					log.Printf("[Coord] Immediately clamped Worker %d target boundary to %d (handed off to Worker %d)", other.id, startOffset, w.id)
				}
			}
		}

		state.ActiveWorkers = append(state.ActiveWorkers, w)
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			runDownloadWorker(wCtx, w, state, initialHeader, file, pipeline, removeWorker, parentCancel)
		}()
	}

	spawnWorker := func(startOffset int64) {
		spawnWorkerAt(startOffset, false)
	}

	spawnTailPrefetchWorker := func(startOffset int64) {
		state.Lock.Lock()
		if state.IsPrehydrating {
			state.Lock.Unlock()
			return
		}
		state.Lock.Unlock()

		state.WorkersLock.Lock()
		defer state.WorkersLock.Unlock()
		workerIdGen++
		wCtx, wCancel := context.WithCancel(parentCtx)
		w := &Worker{
			id:               workerIdGen,
			startOffset:      startOffset,
			cancel:           wCancel,
			currentOffset:    startOffset,
			targetEndOffset:  state.TotalSize,
			isPrefetchHelper: true,
			spawnTimeNano:    time.Now().UnixNano(),
			lastByteTimeNano: time.Now().UnixNano(),
		}
		atomic.StoreInt32(&w.isPrefetching, 1)

		// Immediately clamp any trailing active workers running behind startOffset
		for _, other := range state.ActiveWorkers {
			otherCur := atomic.LoadInt64(&other.currentOffset)
			if otherCur < startOffset {
				otherTarget := atomic.LoadInt64(&other.targetEndOffset)
				if otherTarget <= 0 || otherTarget > startOffset {
					atomic.StoreInt64(&other.targetEndOffset, startOffset)
					log.Printf("[Coord] Immediately clamped Worker %d target boundary to %d (metadata tail Worker %d)", other.id, startOffset, w.id)
				}
			}
		}

		state.ActiveWorkers = append(state.ActiveWorkers, w)
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			runDownloadWorker(wCtx, w, state, initialHeader, file, pipeline, removeWorker, parentCancel)
		}()
	}

	// Initial scan to start worker(s)
	state.Lock.Lock()
	var initialTargetOffset int64 = -1
	for _, starvingOffset := range state.Starving {
		if !state.RangeSet.Contains(starvingOffset) {
			initialTargetOffset = starvingOffset
			break
		}
	}
	state.Lock.Unlock()

	if initialTargetOffset != -1 {
		if initialTargetOffset < state.GetRingFloorOffset() {
			state.LowerRingFloorOffset(initialTargetOffset)
		}
		spawnWorker(initialTargetOffset)
	} else {
		gapStart, _, found := state.RangeSet.FindNextGap(state.GetRingFloorOffset(), state.TotalSize)
		if found {
			spawnWorker(gapStart)
		}
	}

	consecutiveStallBreakouts := 0
	lastTotalBytesDownloaded := atomic.LoadInt64(&state.TotalBytesDownloaded)

	// Main Coordinator Orchestration Loop
	for {
		currentTotalBytes := atomic.LoadInt64(&state.TotalBytesDownloaded)
		if currentTotalBytes > lastTotalBytesDownloaded {
			consecutiveStallBreakouts = 0
			lastTotalBytesDownloaded = currentTotalBytes
		}
		// Sniff check for container optimization (skip during prehydration)
		state.Lock.Lock()
		isPrehydrating := state.IsPrehydrating
		state.Lock.Unlock()

		if !isPrehydrating {
			sniffAndSchedulePrefetch(state, file)
		}

		// Check if fully completed (or all ranges from ringFloorOffset to TotalSize completed in ring-buffer mode)
		if state.RangeSet.IsComplete(state.TotalSize) {
			break
		}
		if state.GetRingFloorOffset() > 0 && state.GetTargetTier() == TierTmp {
			if _, _, found := state.RangeSet.FindNextGap(state.GetRingFloorOffset(), state.TotalSize); !found {
				state.WorkersLock.Lock()
				activeCnt := len(state.ActiveWorkers)
				state.WorkersLock.Unlock()
				if activeCnt == 0 {
					break
				}
			}
		}

		// Keep-alive check: if no workers are running, start one at first gap
		state.WorkersLock.Lock()
		activeCount := len(state.ActiveWorkers)
		state.WorkersLock.Unlock()

		if activeCount == 0 {
			var keepAliveTarget int64 = -1
			state.Lock.Lock()
			for _, sb := range state.Starving {
				if !state.RangeSet.Contains(sb) {
					keepAliveTarget = sb
					break
				}
			}
			state.Lock.Unlock()

			if keepAliveTarget != -1 {
				if keepAliveTarget < state.GetRingFloorOffset() {
					state.LowerRingFloorOffset(keepAliveTarget)
				}
			} else {
				gapStart, _, found := state.RangeSet.FindNextGap(state.GetRingFloorOffset(), state.TotalSize)
				if found {
					keepAliveTarget = gapStart
				}
			}

			if keepAliveTarget != -1 {
				log.Printf("[Coord] No active workers but hydration incomplete. Spawning worker at offset %d", keepAliveTarget)
				spawnWorker(keepAliveTarget)
			}
		}

		select {
		case <-state.NotifyChan:
		case <-state.ManualAddWorkerChan:
			select {
			case state.ManualAddWorkerChan <- struct{}{}:
			default:
			}
		case <-time.After(500 * time.Millisecond):
		case <-parentCtx.Done():
			return
		}

		state.Lock.Lock()
		pendingTail := state.PendingTailPrefetchOffset
		isPrehydrating = state.IsPrehydrating
		if pendingTail > 0 && GlobalConfig.GetMaxUpstreamWorkers() > 1 && !isPrehydrating {
			state.PendingTailPrefetchOffset = -1
			alreadyCached := state.RangeSet.Contains(pendingTail)
			state.Lock.Unlock()
			if !alreadyCached {
				log.Printf("[Coord] Sniffed non-optimized container. Spawning parallel helper worker to pre-fetch metadata starting at offset %d", pendingTail)
				spawnTailPrefetchWorker(pendingTail)
			}
			state.Lock.Lock()
		}

		// Find all starving offsets that are not covered
		var starvingOffsets []int64
		for _, sb := range state.Starving {
			if !state.RangeSet.Contains(sb) {
				alreadyInList := false
				for _, off := range starvingOffsets {
					if off == sb {
						alreadyInList = true
						break
					}
				}
				if !alreadyInList {
					starvingOffsets = append(starvingOffsets, sb)
				}
			}
		}
		state.Lock.Unlock()

		if len(starvingOffsets) > 0 {
			for _, sb := range starvingOffsets {
				stallTimeout := GlobalConfig.GetStarvationStallTimeout()

				state.WorkersLock.Lock()

				removeWorkerLocked := func(wID int) {
					for i, aw := range state.ActiveWorkers {
						if aw.id == wID {
							state.ActiveWorkers = append(state.ActiveWorkers[:i], state.ActiveWorkers[i+1:]...)
							break
						}
					}
				}

				// 1. Classify workers covering sb into healthy vs. stalled
				var healthyWorkers []*Worker
				var stalledWorkers []*Worker

				for _, w := range state.ActiveWorkers {
					wCurrent := atomic.LoadInt64(&w.currentOffset)
					wTargetEnd := atomic.LoadInt64(&w.targetEndOffset)
					if wTargetEnd <= 0 || wTargetEnd > state.TotalSize {
						wTargetEnd = state.TotalSize
					}

					lastByteNano := atomic.LoadInt64(&w.lastByteTimeNano)
					var workerStallDuration time.Duration
					if lastByteNano > 0 {
						workerStallDuration = time.Since(time.Unix(0, lastByteNano))
					} else if w.spawnTimeNano > 0 {
						workerStallDuration = time.Since(time.Unix(0, w.spawnTimeNano))
					}
					isWorkerStalled := stallTimeout > 0 && workerStallDuration >= stallTimeout

					covers := false

					// a) Proximity window (live cursor within 512KB)
					if wCurrent <= sb && sb-wCurrent <= 512*1024 {
						covers = true
					}

					// b) Prefetching metadata
					if !covers && atomic.LoadInt32(&w.isPrefetching) == 1 && sb >= wCurrent {
						covers = true
					}

					// d) Sequential catch-up ETA calculation
					if !covers && wCurrent < sb {
						gapStart, _, found := state.RangeSet.FindNextGap(wCurrent, sb)
						if !found || gapStart == wCurrent {
							wSpeed := atomic.LoadInt64(&w.speed)
							if wSpeed == 0 {
								connStartNano := atomic.LoadInt64(&w.connStartTimeNano)
								connBytes := atomic.LoadInt64(&w.connBytesWritten)
								if connStartNano > 0 && connBytes > 0 {
									elapsedSecs := time.Since(time.Unix(0, connStartNano)).Seconds()
									if elapsedSecs > 0.1 {
										wSpeed = int64(float64(connBytes) / elapsedSecs)
									}
								}
							}

							if wSpeed > 0 {
								hydrationWait := GlobalConfig.GetClientHydrationWait()
								bytesRemaining := sb - wCurrent
								eta := time.Duration(float64(bytesRemaining)/float64(wSpeed)) * time.Second
								if eta < hydrationWait {
									covers = true
								}
							}
						}
					}

					if covers {
						if isWorkerStalled {
							stalledWorkers = append(stalledWorkers, w)
						} else {
							healthyWorkers = append(healthyWorkers, w)
						}
					}
				}

				// Case 1: At least one healthy worker is actively covering sb -> Suppress!
				if len(healthyWorkers) > 0 {
					if GlobalConfig.GetLogVerbose() {
						log.Printf("[Coord] [Verbose] Client Hydration Wait active for offset %d (worker %d is at %d). Suppressing jump.",
							sb, healthyWorkers[0].id, atomic.LoadInt64(&healthyWorkers[0].currentOffset))
					}
					// Clean up any superseded stalled workers on this same offset
					for _, sw := range stalledWorkers {
						log.Printf("[Coord] Reaping redundant stalled worker %d (at offset %d) superseded by active worker %d",
							sw.id, atomic.LoadInt64(&sw.currentOffset), healthyWorkers[0].id)
						sw.cancel()
						removeWorkerLocked(sw.id)
					}
					state.WorkersLock.Unlock()
					continue
				}

				// Case 2: All workers covering sb are stalled (True Stall Breakout!)
				if len(stalledWorkers) > 0 {
					primaryStalled := stalledWorkers[0]
					lastByteNano := atomic.LoadInt64(&primaryStalled.lastByteTimeNano)
					var stallDuration time.Duration
					if lastByteNano > 0 {
						stallDuration = time.Since(time.Unix(0, lastByteNano))
					} else if primaryStalled.spawnTimeNano > 0 {
						stallDuration = time.Since(time.Unix(0, primaryStalled.spawnTimeNano))
					}
					log.Printf("[Coord] Worker %d starved of incoming bytes for %s (>= %s threshold) with client starving at %d. Probing forward for dead-zone bypass...",
						primaryStalled.id, stallDuration.Round(time.Millisecond), stallTimeout, sb)

					for _, sw := range stalledWorkers {
						sw.cancel()
						removeWorkerLocked(sw.id)
					}
					state.WorkersLock.Unlock()

					// Run exponential dead-zone probe forward (outside of locks!)
					goodOffset, perr := probeNextReadableOffsetFn(parentCtx, state.URL, sb, state.TotalSize, state.UpstreamUserAgent)
					spawnTarget := sb
					if perr == nil && goodOffset > sb {
						log.Printf("[Coord] Live dead zone detected [%d, %d). Synthesizing zeroes and launching scout at %d.",
							sb, goodOffset, goodOffset)
						state.RecordStalledGap(sb, goodOffset)
						spawnTarget = goodOffset
					}

					state.Lock.Lock()
					for clientID, starveOff := range state.Starving {
						if starveOff == sb {
							state.StarvingSince[clientID] = time.Now()
						}
					}
					// Wake client readers immediately so CacheReader synthesizes zeroes if gap detected!
					for _, ch := range state.Clients {
						safeSignal(ch)
					}
					state.Lock.Unlock()

					// If probe found a good offset, skip teardown cooldown to give live viewer 0ms buffering!
					if spawnTarget == sb {
						stallCooldown := 5 * time.Second
						if GlobalConfig != nil {
							stallCooldown = GlobalConfig.GetStallTeardownCooldown()
						}
						consecutiveStallBreakouts++
						multiplier := consecutiveStallBreakouts
						if multiplier > 3 {
							multiplier = 3
						}
						actualCooldown := stallCooldown * time.Duration(multiplier)
						log.Printf("[Coord] Starvation breakout teardown cooldown (%s, consecutive #%d) for %s before fresh upstream reconnect",
							actualCooldown, consecutiveStallBreakouts, state.Key)

						coordinatorSleepMu.RLock()
						sleepFn := coordinatorSleepFn
						coordinatorSleepMu.RUnlock()
						if sleepFn != nil {
							sleepFn(actualCooldown)
						} else {
							timer := time.NewTimer(actualCooldown)
							select {
							case <-timer.C:
							case <-parentCtx.Done():
								timer.Stop()
								return
							}
						}
					} else {
						// Dead zone leaped: reset stall breakout counter
						consecutiveStallBreakouts = 0
					}

					state.Lock.Lock()
					active := state.WriterActive
					isYielding := state.IsYielding
					state.Lock.Unlock()
					if !active || isYielding || parentCtx.Err() != nil {
						continue
					}

					spawnWorker(spawnTarget)
					continue
				}

				// Case 3: No worker covers sb (player seek or uncovered gap)
				activeCount = len(state.ActiveWorkers)
				maxWorkers := GlobalConfig.GetMaxUpstreamWorkers()
				if activeCount < maxWorkers {
					state.WorkersLock.Unlock()
					if sb < state.GetRingFloorOffset() {
						state.LowerRingFloorOffset(sb)
					}
					log.Printf("[Coord] Spawning new worker starting at starving offset %d (Active: %d/%d)", sb, activeCount+1, maxWorkers)
					spawnWorker(sb)
					continue
				}

				// Worker limit reached. Find worker to pre-empt:
				// Priority 1: Pick any worker in ActiveWorkers that is stalled
				var workerToPreempt *Worker
				for _, w := range state.ActiveWorkers {
					if atomic.LoadInt32(&w.isPrefetching) != 0 {
						continue
					}
					lastByteNano := atomic.LoadInt64(&w.lastByteTimeNano)
					var wStall time.Duration
					if lastByteNano > 0 {
						wStall = time.Since(time.Unix(0, lastByteNano))
					} else if w.spawnTimeNano > 0 {
						wStall = time.Since(time.Unix(0, w.spawnTimeNano))
					}
					if stallTimeout > 0 && wStall >= stallTimeout {
						workerToPreempt = w
						break
					}
				}

				// Priority 2: If no stalled worker, find furthest worker past minTime
				if workerToPreempt == nil {
					maxDistance := int64(-1)
					minTime := GlobalConfig.GetSchedulerMinTime()
					for _, w := range state.ActiveWorkers {
						if atomic.LoadInt32(&w.isPrefetching) != 0 {
							continue
						}
						wCurrent := atomic.LoadInt64(&w.currentOffset)
						connStartNano := atomic.LoadInt64(&w.connStartTimeNano)
						var elapsed time.Duration
						if connStartNano > 0 {
							elapsed = time.Since(time.Unix(0, connStartNano))
						} else if w.spawnTimeNano > 0 {
							elapsed = time.Since(time.Unix(0, w.spawnTimeNano))
						}
						if minTime <= 0 || elapsed >= minTime {
							var minDistanceToClient int64 = 1e18
							for _, clientStarveOffset := range starvingOffsets {
								dist := clientStarveOffset - wCurrent
								if dist < 0 {
									dist = -dist
								}
								if dist < minDistanceToClient {
									minDistanceToClient = dist
								}
							}
							if minDistanceToClient > maxDistance {
								maxDistance = minDistanceToClient
								workerToPreempt = w
							}
						}
					}
				}

				if workerToPreempt != nil {
					log.Printf("[Coord] Pre-empting worker %d (at offset %d) to switch to starving offset %d",
						workerToPreempt.id, atomic.LoadInt64(&workerToPreempt.currentOffset), sb)
					workerToPreempt.cancel()
					removeWorkerLocked(workerToPreempt.id)
					state.WorkersLock.Unlock()

					state.Lock.Lock()
					for clientID, starveOff := range state.Starving {
						if starveOff == sb {
							state.StarvingSince[clientID] = time.Now()
						}
					}
					state.Lock.Unlock()

					if sb < state.GetRingFloorOffset() {
						state.LowerRingFloorOffset(sb)
					}
					spawnWorker(sb)
				} else {
					state.WorkersLock.Unlock()
				}
			}
		}

		// Handle explicit manual +Worker request when under MaxUpstreamWorkers and no skip is pending.
		manualRequested := false
		if state.ManualAddWorkerChan != nil {
			select {
			case <-state.ManualAddWorkerChan:
				manualRequested = true
			default:
			}
		}
		if manualRequested && len(starvingOffsets) == 0 && !state.RangeSet.IsComplete(state.TotalSize) {
			maxWorkers := GlobalConfig.GetMaxUpstreamWorkers()
			if maxWorkers > 1 {
				state.WorkersLock.Lock()
				curWorkers := len(state.ActiveWorkers)
				state.WorkersLock.Unlock()
				if curWorkers < maxWorkers {
					if mid, gs, ge, ok := findLargestGapMidpoint(state); ok {
						log.Printf("[Coord] Manual +Worker spawning at midpoint %d of largest remaining gap [%d, %d) (Active: %d/%d)",
							mid, gs, ge, curWorkers+1, maxWorkers)
						spawnWorkerAt(mid, true)
					}
				}
			}
		}
	}
}

func hasUncoveredStarvingLocked(state *FileCacheState) bool {
	if state == nil || state.RangeSet == nil {
		return false
	}
	for _, sb := range state.Starving {
		if !state.RangeSet.Contains(sb) {
			return true
		}
	}
	return false
}

// findLargestGapMidpoint returns the midpoint of the largest un-downloaded gap starting at
// ringFloorOffset, splitting gaps around active workers' current offsets so a manually added
// worker never lands on top of an already-advancing worker.
func findLargestGapMidpoint(state *FileCacheState) (mid int64, gapStart int64, gapEnd int64, ok bool) {
	if state == nil || state.TotalSize <= 0 || state.RangeSet == nil {
		return 0, 0, 0, false
	}
	floor := state.GetRingFloorOffset()
	if floor < 0 {
		floor = 0
	}
	if floor >= state.TotalSize {
		return 0, 0, 0, false
	}

	type span struct{ start, end int64 }
	var gaps []span
	cursor := floor
	for cursor < state.TotalSize {
		gs, ge, found := state.RangeSet.FindNextGap(cursor, state.TotalSize)
		if !found || ge <= gs {
			break
		}
		gaps = append(gaps, span{start: gs, end: ge})
		cursor = ge
	}
	if len(gaps) == 0 {
		return 0, 0, 0, false
	}

	state.WorkersLock.Lock()
	cursors := make([]int64, 0, len(state.ActiveWorkers))
	for _, w := range state.ActiveWorkers {
		if w != nil {
			cursors = append(cursors, atomic.LoadInt64(&w.currentOffset))
		}
	}
	state.WorkersLock.Unlock()
	sort.Slice(cursors, func(i, j int) bool { return cursors[i] < cursors[j] })

	var bestStart, bestEnd, bestLen int64
	for _, g := range gaps {
		s := g.start
		for _, c := range cursors {
			if c > s && c < g.end {
				if c-s > bestLen {
					bestLen = c - s
					bestStart, bestEnd = s, c
				}
				s = c
			}
		}
		if g.end-s > bestLen {
			bestLen = g.end - s
			bestStart, bestEnd = s, g.end
		}
	}

	minSplit := int64(2)
	if state.TotalSize >= 4*1024*1024 {
		minSplit = 1024 * 1024
	}
	if bestLen < minSplit {
		return 0, 0, 0, false
	}
	mid = bestStart + bestLen/2
	if cm := state.GetChunkManager(); cm != nil {
		if chunkSize := cm.ChunkSize(); chunkSize > 0 {
			aligned := ((mid + chunkSize/2) / chunkSize) * chunkSize
			if aligned <= bestStart {
				aligned = ((bestStart / chunkSize) + 1) * chunkSize
			} else if aligned >= bestEnd {
				aligned = ((bestEnd - 1) / chunkSize) * chunkSize
			}
			if aligned > bestStart && aligned < bestEnd {
				mid = aligned
			}
		}
	}
	return mid, bestStart, bestEnd, true
}

func runDownloadWorker(
	ctx context.Context,
	w *Worker,
	state *FileCacheState,
	initialHeader http.Header,
	file *os.File,
	pipeline StorageSink,
	onExit func(*Worker),
	parentCancel context.CancelFunc,
) {
	defer onExit(w)
	log.Printf("[Worker-%d] Starting background downloader at offset %d", w.id, w.startOffset)

	state.Lock.Lock()
	cm := state.ChunkManager
	state.Lock.Unlock()

	consecutiveFailures := 0
	backoff := 2 * time.Second
	currentOffset := w.startOffset

	handleFailure := func(isPermanent bool, errFormat string, args ...interface{}) bool {
		if ctx.Err() != nil {
			log.Printf("[Worker-%d] Context canceled. Exiting worker.", w.id)
			return true
		}
		log.Printf(errFormat, args...)
		if isPermanent {
			log.Printf("[Worker-%d] Permanent failure. Exiting worker.", w.id)
			parentCancel()
			return true
		}

		select {
		case <-ctx.Done():
			return true
		default:
		}

		consecutiveFailures++

		// Background Prehydration Dead-Zone Skip: If worker fails 3 consecutive retries at the exact same offset,
		// probe forward to bypass the dead sector and continue downloading downstream video!
		if consecutiveFailures >= 3 && currentOffset < state.TotalSize {
			log.Printf("[Worker-%d] Worker reached %d consecutive failures at offset %d. Probing forward for dead zone...",
				w.id, consecutiveFailures, currentOffset)
			goodOffset, err := probeNextReadableOffsetFn(ctx, state.URL, currentOffset, state.TotalSize, state.UpstreamUserAgent)
			if err == nil && goodOffset > currentOffset {
				log.Printf("[Worker-%d] Dead zone bypassed [%d, %d). Leapfrogging worker to %d.",
					w.id, currentOffset, goodOffset, goodOffset)
				state.RecordStalledGap(currentOffset, goodOffset)
				currentOffset = goodOffset
				atomic.StoreInt64(&w.currentOffset, currentOffset)
				consecutiveFailures = 0
				backoff = 2 * time.Second
				return false
			}
		}

		maxRetries := GlobalConfig.GetMaxUpstreamRetries()
		if maxRetries > 0 && consecutiveFailures >= maxRetries {
			log.Printf("[Worker-%d] Maximum consecutive failures reached (%d/%d). Exiting worker.", w.id, consecutiveFailures, maxRetries)

			state.WorkersLock.Lock()
			activeCount := len(state.ActiveWorkers)
			state.WorkersLock.Unlock()

			if activeCount <= 1 {
				parentCancel()
			}
			return true
		}

		if maxRetries > 0 {
			log.Printf("[Worker-%d] Retry %d/%d. Sleeping %s before retrying...", w.id, consecutiveFailures, maxRetries, backoff)
		} else {
			log.Printf("[Worker-%d] Retry %d (unlimited retries). Sleeping %s before retrying...", w.id, consecutiveFailures, backoff)
		}

		state.SetUpstreamStatus("Retrying")

		coordinatorSleepMu.RLock()
		sleepFn := coordinatorSleepFn
		coordinatorSleepMu.RUnlock()

		if sleepFn == nil {
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				log.Printf("[Worker-%d] Context canceled during retry backoff. Exiting worker.", w.id)
				return true
			case <-timer.C:
				if ctx.Err() != nil {
					log.Printf("[Worker-%d] Context canceled during retry backoff. Exiting worker.", w.id)
					return true
				}
			}
		} else {
			done := make(chan struct{})
			go func() {
				sleepFn(backoff)
				close(done)
			}()

			select {
			case <-ctx.Done():
				log.Printf("[Worker-%d] Context canceled during retry backoff. Exiting worker.", w.id)
				return true
			case <-done:
				if ctx.Err() != nil {
					log.Printf("[Worker-%d] Context canceled during retry backoff. Exiting worker.", w.id)
					return true
				}
			}
		}

		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
		return false
	}

	connID := ""
	hadFailure := false

	var activeRespBody io.ReadCloser
	closeConnection := func() {
		if activeRespBody != nil {
			_ = activeRespBody.Close()
			activeRespBody = nil
			log.Printf("[Worker-%d] Closed active upstream connection", w.id)
			if connID != "" {
				state.RecordConnEnd(connID)
				connID = ""
			}
		}
	}
	defer closeConnection()

	// Pre-flight check: if the assigned start offset is already satisfied on disk, find next unclaimed gap
	if state.RangeSet.Contains(currentOffset) && !w.isPrefetchHelper {
		gapStart, gapEnd, found := state.FindNextUnclaimedGap(w.id, currentOffset)
		if !found {
			log.Printf("[Worker-%d] Start offset %d already satisfied and no unclaimed gaps remain. Exiting.", w.id, currentOffset)
			return
		}
		currentOffset = gapStart
		atomic.StoreInt64(&w.currentOffset, currentOffset)
		atomic.StoreInt64(&w.targetEndOffset, gapEnd)
	}

	buf := make([]byte, 128*1024)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Single-worker inline tail prefetch check
		state.Lock.Lock()
		pendingTail := state.PendingTailPrefetchOffset
		if pendingTail > 0 && GlobalConfig.GetMaxUpstreamWorkers() == 1 {
			state.PendingTailPrefetchOffset = -1
		} else {
			pendingTail = -1
		}
		state.Lock.Unlock()

		if pendingTail > 0 {
			closeConnection()
			runInlineTailPrefetch(ctx, w, state, initialHeader, file, pipeline, pendingTail)
			continue
		}

		// Check if current offset is already covered or past target end
		targetEnd := atomic.LoadInt64(&w.targetEndOffset)
		if !w.isPrefetchHelper && (targetEnd <= currentOffset || state.RangeSet.Contains(currentOffset)) {
			gapStart, gapEnd, found := state.FindNextUnclaimedGap(w.id, currentOffset)
			if !found {
				log.Printf("[Worker-%d] All remaining gaps completed or in-flight. Worker coalescing and exiting.", w.id)
				closeConnection()
				return
			}
			log.Printf("[Worker-%d] Reached completed range or boundary at %d. Leapfrogging to next unclaimed gap at [%d, %d)", w.id, currentOffset, gapStart, gapEnd)
			closeConnection()
			currentOffset = gapStart
			atomic.StoreInt64(&w.currentOffset, currentOffset)
			atomic.StoreInt64(&w.targetEndOffset, gapEnd)
			atomic.StoreInt64(&w.lastByteTimeNano, time.Now().UnixNano())
		}

		if currentOffset >= state.TotalSize {
			closeConnection()
			return
		}

		if activeRespBody == nil {
			req, err := http.NewRequestWithContext(ctx, "GET", state.URL, nil)
			if err != nil {
				if handleFailure(false, "[Worker-%d] Error creating request: %v", w.id, err) {
					return
				}
				continue
			}

			reqEnd := atomic.LoadInt64(&w.targetEndOffset)
			if reqEnd <= currentOffset || reqEnd > state.TotalSize {
				reqEnd = state.TotalSize
			}

			rangeHeader := fmt.Sprintf("bytes=%d-%d", currentOffset, reqEnd-1)
			req.Header.Set("Range", rangeHeader)

			CopySafeHeaders(initialHeader, req.Header)
			if ua := req.Header.Get("User-Agent"); ua != "" {
				state.Lock.Lock()
				state.UpstreamUserAgent = ua
				state.Lock.Unlock()
			}

			state.SetUpstreamStatus("Connecting")

			log.Printf("[Worker-%d] Connecting upstream for offset %d (Range: %s)", w.id, currentOffset, rangeHeader)
			resp, err := UpstreamHTTPClient.Do(req)
			if err != nil {
				hadFailure = true
				if handleFailure(false, "[Worker-%d] Upstream connection failed: %v", w.id, err) {
					return
				}
				continue
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				hadFailure = true
				_ = resp.Body.Close()
				state.Lock.Lock()
				isPrehydrating := state.IsPrehydrating
				isMaterialized := state.DiskMaterialized
				state.Lock.Unlock()
				if isPrehydrating && (resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusForbidden) {
					if GlobalPrehydrationManager != nil {
						GlobalPrehydrationManager.YieldState(state, fmt.Sprintf("HTTP %d upstream backpressure", resp.StatusCode))
					}
					return
				}
				isPermanent := !isMaterialized && (resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone)
				if handleFailure(isPermanent, "[Worker-%d] Upstream returned error status %d", w.id, resp.StatusCode) {
					return
				}
				continue
			}

			// Pre-flight check after connection: did another worker finish this range while we handshook?
			if state.RangeSet.Contains(currentOffset) && !w.isPrefetchHelper {
				log.Printf("[Worker-%d] Offset %d satisfied during connect handshake. Cleanly aborting connection.", w.id, currentOffset)
				_ = resp.Body.Close()
				gapStart, gapEnd, found := state.FindNextUnclaimedGap(w.id, currentOffset)
				if !found {
					log.Printf("[Worker-%d] All remaining gaps completed or in-flight. Worker coalescing and exiting.", w.id)
					return
				}
				currentOffset = gapStart
				atomic.StoreInt64(&w.currentOffset, currentOffset)
				atomic.StoreInt64(&w.targetEndOffset, gapEnd)
				atomic.StoreInt64(&w.lastByteTimeNano, time.Now().UnixNano())
				continue
			}

			activeRespBody = resp.Body
			state.SetUpstreamStatus("Downloading")

			consecutiveFailures = 0
			backoff = 2 * time.Second

			atomic.StoreInt64(&w.connStartTimeNano, time.Now().UnixNano())
			atomic.StoreInt64(&w.lastByteTimeNano, time.Now().UnixNano())
			atomic.StoreInt64(&w.connBytesWritten, 0)

			connID = fmt.Sprintf("worker-%d-%d", w.id, time.Now().UnixNano())
			state.RecordConnStart(connID)

			if hadFailure {
				state.Lock.Lock()
				var lastClosed time.Time
				for _, c := range state.RecentConnections {
					if !c.Closed.IsZero() {
						if lastClosed.IsZero() || c.Closed.After(lastClosed) {
							lastClosed = c.Closed
						}
					}
				}
				if !lastClosed.IsZero() {
					elapsed := time.Since(lastClosed).Seconds()
					state.ObservedCooldown = elapsed
					log.Printf("[Coord] Observed cooldown from last closed connection: %.2fs", elapsed)
				}
				state.Lock.Unlock()
				hadFailure = false
			}
		}

		// Wait if scratch disk budget is exceeded or disk free space is low (TCP socket backpressure)
		if cm != nil {
			if berr := cm.WaitScratchBudget(ctx); berr != nil {
				closeConnection()
				return
			}
		}

		// Read streaming chunk from upstream HTTP body
		readStartTime := time.Now()
		n, err := activeRespBody.Read(buf)
		if n > 0 {
			// Materialize to disk BEFORE WriteChunk if buffered bytes or LiveBuffer capacity exceed threshold
			// so LiveBuffer eviction never discards unpersisted chunks while wp.file == nil.
			matThreshold := int64(32 * 1024 * 1024)
			if cm != nil && cm.maxInFlightBytes > 0 && cm.maxInFlightBytes/2 < matThreshold {
				matThreshold = cm.maxInFlightBytes / 2
			}
			liveBufFull := state.LiveBuffer != nil && state.LiveBuffer.maxBytes > 0 && state.LiveBuffer.GetCurBytes()+int64(n) > state.LiveBuffer.maxBytes
			if !state.IsDiskMaterialized() && (state.RangeSet.TotalBytes()+int64(n) >= matThreshold || liveBufFull) {
				if GlobalRegistry != nil && state.TotalSize > 0 && state.GetTargetTier() == TierDisk {
					GlobalRegistry.PreEvict(state.TotalSize)
				}
				_ = state.MaterializeDiskFile()
				state.Lock.Lock()
				if state.File != nil {
					file = state.File
				}
				state.Lock.Unlock()
			}

			var werr error
			if pipeline != nil {
				werr = pipeline.WriteChunk(currentOffset, buf[:n])
			} else if file != nil {
				state.partIOMu.RLock()
				_, werr = file.WriteAt(buf[:n], currentOffset)
				if werr == nil && state.DiskRangeSet != nil {
					state.DiskRangeSet.Add(currentOffset, currentOffset+int64(n))
				}
				state.partIOMu.RUnlock()
			}
			if werr != nil {
				log.Printf("[Worker-%d] File write error at %d: %v", w.id, currentOffset, werr)
				closeConnection()
				isPermanent := ctx.Err() != nil || errors.Is(werr, os.ErrClosed)
				if handleFailure(isPermanent, "[Worker-%d] File write error at %d: %v", w.id, currentOffset, werr) {
					return
				}
				continue
			}

			// Add to RangeSet and wake waiting readers immediately!
			newMediaBytes := state.RangeSet.AddAndCount(currentOffset, currentOffset+int64(n))
			if newMediaBytes > 0 {
				atomic.StoreInt64(&state.LastProgressTimeNano, time.Now().UnixNano())
			}
			if cm != nil {
				if newMediaBytes > 0 {
					cm.AddInFlightBytes(newMediaBytes)
				}
				cm.CheckAndTriggerUploads()
			}

			state.Lock.Lock()
			for _, ch := range state.Clients {
				safeSignal(ch)
			}
			sniffed := state.Sniffed
			state.Lock.Unlock()

			if !sniffed {
				sniffAndSchedulePrefetch(state, file)
			}

			currentOffset += int64(n)
			atomic.StoreInt64(&w.currentOffset, currentOffset)
			atomic.AddInt64(&w.connBytesWritten, int64(n))
			atomic.StoreInt64(&w.lastByteTimeNano, time.Now().UnixNano())

			recordDownloadedBytes(state, int64(n))

			readElapsed := time.Since(readStartTime).Seconds()
			if readElapsed > 0 {
				instantSpeed := int64(float64(n) / readElapsed)
				atomic.StoreInt64(&w.speed, instantSpeed)
			}

			// In-flight collision & boundary check: check if worker reached target end or cached range
			targetEnd := atomic.LoadInt64(&w.targetEndOffset)
			if !w.isPrefetchHelper && (currentOffset >= targetEnd || state.RangeSet.Contains(currentOffset)) {
				log.Printf("[Worker-%d] Reached target boundary %d at offset %d. Leapfrogging to next unclaimed gap...", w.id, targetEnd, currentOffset)
				closeConnection()
				gapStart, gapEnd, found := state.FindNextUnclaimedGap(w.id, currentOffset)
				if !found {
					log.Printf("[Worker-%d] All remaining gaps completed or in-flight. Worker coalescing and exiting.", w.id)
					return
				}
				currentOffset = gapStart
				atomic.StoreInt64(&w.currentOffset, currentOffset)
				atomic.StoreInt64(&w.targetEndOffset, gapEnd)
				atomic.StoreInt64(&w.lastByteTimeNano, time.Now().UnixNano())
				continue
			}
		}

		if err != nil {
			if err != io.EOF {
				log.Printf("[Worker-%d] Upstream read error at %d: %v", w.id, currentOffset, err)
			}
			closeConnection()

			if currentOffset >= state.TotalSize || state.RangeSet.IsComplete(state.TotalSize) {
				log.Printf("[Worker-%d] Completed full download to EOF (%d bytes). Exiting.", w.id, currentOffset)
				return
			}

			if w.isPrefetchHelper {
				log.Printf("[Worker-%d] Prefetch helper completed read up to %d.", w.id, currentOffset)
				return
			}

			// Clean bounded range completion check on EOF
			if err == io.EOF {
				targetEnd := atomic.LoadInt64(&w.targetEndOffset)
				if currentOffset >= targetEnd || state.RangeSet.Contains(currentOffset) {
					log.Printf("[Worker-%d] Bounded upstream stream completed cleanly to %d. Leapfrogging to next unclaimed gap...", w.id, currentOffset)
					gapStart, gapEnd, found := state.FindNextUnclaimedGap(w.id, currentOffset)
					if !found {
						log.Printf("[Worker-%d] All remaining gaps completed or in-flight. Worker coalescing and exiting.", w.id)
						return
					}
					currentOffset = gapStart
					atomic.StoreInt64(&w.currentOffset, currentOffset)
					atomic.StoreInt64(&w.targetEndOffset, gapEnd)
					atomic.StoreInt64(&w.lastByteTimeNano, time.Now().UnixNano())
					continue
				}
			}

			if handleFailure(false, "[Worker-%d] Upstream read error at %d: %v", w.id, currentOffset, err) {
				return
			}
			continue
		}
	}
}

// FindNextUnclaimedGap searches for the next un-downloaded byte gap that is NOT currently
// being downloaded in-flight by any other active worker.
func (s *FileCacheState) FindNextUnclaimedGap(workerID int, fromOffset int64) (start, end int64, found bool) {
	if s.TotalSize <= 0 {
		return 0, 0, false
	}

	// 1. Gather all in-flight ranges claimed by other active workers
	type inFlightRange struct {
		start int64
		end   int64
	}
	var inFlight []inFlightRange
	s.WorkersLock.Lock()
	for _, other := range s.ActiveWorkers {
		if other.id != workerID {
			cur := atomic.LoadInt64(&other.currentOffset)
			targetEnd := atomic.LoadInt64(&other.targetEndOffset)
			if targetEnd <= 0 || targetEnd > s.TotalSize {
				targetEnd = s.TotalSize
			}
			if cur < targetEnd {
				inFlight = append(inFlight, inFlightRange{start: cur, end: targetEnd})
			}
		}
	}
	s.WorkersLock.Unlock()

	// Sort in-flight ranges ascending by start offset
	sort.Slice(inFlight, func(i, j int) bool {
		return inFlight[i].start < inFlight[j].start
	})

	isClaimed := func(gStart, gEnd int64) (bool, int64, int64) {
		for _, ifr := range inFlight {
			// If gap overlaps with an in-flight worker range:
			if gStart < ifr.end && gEnd > ifr.start {
				if gStart < ifr.start {
					// There is an un-claimed segment before ifr begins!
					return false, gStart, ifr.start
				}
				// Otherwise this gap is claimed at gStart; un-claimed resumes at ifr.end
				return true, gStart, ifr.end
			}
		}
		return false, gStart, gEnd
	}

	// 2. Search forward from max(fromOffset, ringFloorOffset) to TotalSize
	floor := s.GetRingFloorOffset()
	curr := fromOffset
	if curr < floor {
		curr = floor
	}
	for curr < s.TotalSize {
		gStart, gEnd, ok := s.RangeSet.FindNextGap(curr, s.TotalSize)
		if !ok {
			break
		}
		claimed, freeStart, freeEnd := isClaimed(gStart, gEnd)
		if !claimed && freeEnd > freeStart {
			return freeStart, freeEnd, true
		}
		if freeEnd > curr {
			curr = freeEnd
		} else {
			curr = gEnd
		}
	}

	// 3. Wrap around from ringFloorOffset to fromOffset for unfulfilled backfill gaps
	if fromOffset > floor {
		curr = floor
		for curr < fromOffset {
			gStart, gEnd, ok := s.RangeSet.FindNextGap(curr, fromOffset)
			if !ok {
				break
			}
			claimed, freeStart, freeEnd := isClaimed(gStart, gEnd)
			if !claimed && freeEnd > freeStart {
				return freeStart, freeEnd, true
			}
			if freeEnd > curr {
				curr = freeEnd
			} else {
				curr = gEnd
			}
		}
	}

	return 0, 0, false
}

func safeSignal(ch chan struct{}) {
	defer func() {
		_ = recover()
	}()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func runInlineTailPrefetch(
	ctx context.Context,
	w *Worker,
	state *FileCacheState,
	initialHeader http.Header,
	file *os.File,
	pipeline StorageSink,
	tailStartOffset int64,
) {
	prefetchLength := state.TotalSize - tailStartOffset
	if prefetchLength <= 0 {
		return
	}

	atomic.StoreInt32(&w.isPrefetching, 1)
	atomic.StoreInt64(&w.currentOffset, tailStartOffset)
	defer atomic.StoreInt32(&w.isPrefetching, 0)

	state.Lock.Lock()
	cm := state.ChunkManager
	state.Lock.Unlock()

	log.Printf("[Worker-%d] MaxUpstreamWorkers=1: Executing inline tail pre-fetch (%d bytes) starting at offset %d", w.id, prefetchLength, tailStartOffset)

	req, err := http.NewRequestWithContext(ctx, "GET", state.URL, nil)
	if err != nil {
		log.Printf("[Worker-%d] Inline tail pre-fetch request creation failed: %v", w.id, err)
		return
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", tailStartOffset, state.TotalSize-1)
	req.Header.Set("Range", rangeHeader)
	CopySafeHeaders(initialHeader, req.Header)

	state.SetUpstreamStatus("Connecting")

	resp, rerr := UpstreamHTTPClient.Do(req)
	if rerr != nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent) {
		if rerr == nil {
			_ = resp.Body.Close()
		}
		log.Printf("[Worker-%d] Inline tail pre-fetch request failed. Status: %v, err: %v", w.id, resp, rerr)
		return
	}
	defer resp.Body.Close()

	state.SetUpstreamStatus("Downloading")
	buf := make([]byte, 128*1024)
	currentOffset := tailStartOffset

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := resp.Body.Read(buf)
		if n > 0 {
			if !state.IsDiskMaterialized() && state.LiveBuffer != nil && state.LiveBuffer.maxBytes > 0 && state.LiveBuffer.GetCurBytes()+int64(n) > state.LiveBuffer.maxBytes {
				if GlobalRegistry != nil && state.TotalSize > 0 && state.GetTargetTier() == TierDisk {
					GlobalRegistry.PreEvict(state.TotalSize)
				}
				_ = state.MaterializeDiskFile()
				state.Lock.Lock()
				if state.File != nil {
					file = state.File
				}
				state.Lock.Unlock()
			}

			var werr error
			if pipeline != nil {
				werr = pipeline.WriteChunk(currentOffset, buf[:n])
			} else if file != nil {
				state.partIOMu.RLock()
				_, werr = file.WriteAt(buf[:n], currentOffset)
				if werr == nil && state.DiskRangeSet != nil {
					state.DiskRangeSet.Add(currentOffset, currentOffset+int64(n))
				}
				state.partIOMu.RUnlock()
			}
			if werr != nil {
				log.Printf("[Worker-%d] Inline tail pre-fetch write error at %d: %v", w.id, currentOffset, werr)
				break
			}

			newMediaBytes := state.RangeSet.AddAndCount(currentOffset, currentOffset+int64(n))
			if newMediaBytes > 0 {
				atomic.StoreInt64(&state.LastProgressTimeNano, time.Now().UnixNano())
			}
			if cm != nil {
				cm.CheckAndTriggerUploads()
			}
			state.Lock.Lock()
			for _, ch := range state.Clients {
				safeSignal(ch)
			}
			state.Lock.Unlock()

			currentOffset += int64(n)
			atomic.StoreInt64(&w.currentOffset, currentOffset)

			recordDownloadedBytes(state, int64(n))
		}

		if err != nil {
			break
		}
	}

	log.Printf("[Worker-%d] Inline tail pre-fetch finished. Resuming sequential stream.", w.id)
}

func sniffAndSchedulePrefetch(state *FileCacheState, file *os.File) {
	state.Lock.Lock()
	if state.Sniffed || state.IsPrehydrating {
		state.Lock.Unlock()
		return
	}

	avail := state.RangeSet.AvailableFrom(0)
	isHeaderReady := avail >= 128*1024 || (state.TotalSize > 0 && avail >= state.TotalSize)
	if !isHeaderReady {
		state.Lock.Unlock()
		return
	}
	lb := state.LiveBuffer
	partPath := state.PartPath
	finalPath := state.Path
	state.Lock.Unlock()

	buf := make([]byte, 128*1024)
	n := 0
	if lb != nil {
		n, _ = lb.ReadAt(buf, 0)
	}
	if n == 0 && file != nil {
		n, _ = file.ReadAt(buf, 0)
	}
	if n == 0 {
		for _, p := range []string{partPath, finalPath} {
			if p == "" {
				continue
			}
			if f, err := os.Open(p); err == nil {
				n, _ = f.ReadAt(buf, 0)
				_ = f.Close()
				if n > 0 {
					break
				}
			}
		}
	}
	if n > 0 {
		state.Lock.Lock()
		state.Sniffed = true
		state.Lock.Unlock()
		log.Printf("[Coord] Initial header bytes available. Sniffing media container...")
		data := buf[:n]
		isFastStart := false
		moovOffset := int64(0)
		hasMdatSize := false
		hasCues := false
		cuesOffset := int64(0)

		isMkv := n >= 4 && binary.BigEndian.Uint32(data[0:4]) == 0x1A45DFA3
		isMp4 := false

		if isMkv {
			hasCues, cuesOffset = GetMkvCuesOffset(data)
			state.Lock.Lock()
			state.ContentType = "video/x-matroska"
			state.Lock.Unlock()
		} else if n >= 8 {
			firstBoxType := string(data[4:8])
			if firstBoxType == "ftyp" || firstBoxType == "free" || firstBoxType == "wide" || firstBoxType == "skip" || firstBoxType == "moov" || firstBoxType == "mdat" {
				isMp4 = true
				isFastStart, moovOffset, hasMdatSize = IsMp4FastStart(data)
				state.Lock.Lock()
				state.ContentType = "video/mp4"
				state.Lock.Unlock()
			}
		}

		var tailStartOffset int64 = -1
		if isMp4 && !isFastStart && hasMdatSize && moovOffset > 0 {
			tailStartOffset = moovOffset
		} else if isMkv && hasCues && cuesOffset > 0 {
			tailStartOffset = cuesOffset
		}

		if tailStartOffset > 0 && tailStartOffset < state.TotalSize && tailStartOffset > avail {
			tailBytes := state.TotalSize - tailStartOffset
			if tailBytes > 0 {
				if !state.RangeSet.Contains(tailStartOffset) {
					state.Lock.Lock()
					state.PendingTailPrefetchOffset = tailStartOffset
					state.Lock.Unlock()
					safeSignal(state.NotifyChan)
					log.Printf("[Coord] Non-faststart container detected. Scheduled exact tail prefetch from byte offset %d (%d bytes)", tailStartOffset, tailBytes)
				} else {
					log.Printf("[Coord] Metadata tail bytes already cached. Skipping pre-fetch.")
				}
			}
		} else if isFastStart || (isMp4 && moovOffset == 0) {
			log.Printf("[Coord] Fast-start container detected. Metadata localized in header.")
		}
	}
}

func recordDownloadedBytes(state *FileCacheState, n int64) {
	if state == nil || n <= 0 {
		return
	}
	atomic.AddInt64(&state.BytesDownloadedSec, n)
	atomic.AddInt64(&state.TotalBytesDownloaded, n)
	if state.GetTargetTier() == TierDisk && GlobalRegistry != nil {
		atomic.AddInt64(&state.AccountedCacheBytes, n)
		GlobalRegistry.UpdateSize(n)
	}
}

func releaseAccountedCacheBytes(state *FileCacheState) {
	if state == nil || GlobalRegistry == nil {
		return
	}
	accounted := atomic.SwapInt64(&state.AccountedCacheBytes, 0)
	if accounted > 0 {
		GlobalRegistry.UpdateSize(-accounted)
	}
}

func triggerAsyncTargetCleanup(cm *ScratchChunkManager, storage StorageBackend, key string, logPrefix string) {
	if key == "" {
		return
	}
	if cm != nil {
		go func() {
			delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := cm.Cleanup(delCtx); err != nil && !errors.Is(err, ErrStorageCircuitOpen) {
				log.Printf("%s Async target cleanup (%s) failed for %s: %v", logPrefix, cm.TargetName(), key, err)
			}
		}()
		return
	}
	triggerAsyncChunkCleanup(storage, key, logPrefix)
}

func triggerAsyncChunkCleanup(storage StorageBackend, key string, logPrefix string) {
	cleaner, ok := storage.(ChunkCleanupBackend)
	if !ok || key == "" {
		return
	}
	go func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cleaner.DeleteChunks(delCtx, key); err != nil {
			log.Printf("%s Async DeleteChunks failed for %s: %v", logPrefix, key, err)
		}
	}()
}

func finalizeAndCommitCache(state *FileCacheState, fileToClose *os.File, pipeline StorageSink) {
	commitCompletedFile(state, fileToClose, pipeline, true, "[Coord]")
}

func commitCompletedFile(state *FileCacheState, fileToClose *os.File, pipeline StorageSink, earlyPrehydrateRelease bool, logPrefix string) {
	if state == nil {
		return
	}
	defer func() {
		state.Lock.Lock()
		state.WriterActive = false
		state.IsCommitting = false
		state.Lock.Unlock()
	}()

	state.Lock.Lock()
	if fileToClose == nil && state.File != nil {
		fileToClose = state.File
		state.File = nil
	}
	if pipeline == nil && state.Pipeline != nil {
		pipeline = state.Pipeline
		state.Pipeline = nil
	}
	if state.LiveBuffer != nil {
		state.LiveBuffer.Clear()
		state.LiveBuffer = nil
	}
	state.Lock.Unlock()

	if pipeline != nil {
		if err := pipeline.FlushAndClose(); err != nil {
			log.Printf("%s Error flushing writeback pipeline during commit: %v", logPrefix, err)
		}
	}

	if fileToClose != nil {
		FadviseDontNeed(fileToClose.Fd(), 0, 0)
		if err := fileToClose.Close(); err != nil {
			log.Printf("%s Error closing part file during commit: %v", logPrefix, err)
		}
	}

	meta := FileMeta{
		URL:           state.URL,
		TotalSize:     state.TotalSize,
		ContentType:   state.ContentType,
		ETag:          state.ETag,
		LastModified:  state.LastModified,
		LastValidated: time.Now().Unix(),
		Completed:     true,
	}

	tier := state.GetTargetTier()

	if tier == TierTmp {
		state.Lock.Lock()
		state.WriterActive = false
		state.IsCommitting = false
		state.UpstreamStatus = "Completed"
		hasClients := len(state.Clients) > 0
		var cmToClose *ScratchChunkManager
		if !hasClients {
			cmToClose = state.ChunkManager
			state.ChunkManager = nil
		}
		state.Lock.Unlock()

		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}

		if !hasClients {
			if cmToClose != nil {
				cmToClose.Close()
			}
			_ = os.Remove(state.PartPath)
			if state.MetaPath != "" {
				_ = os.Remove(state.MetaPath)
			}
			releaseAccountedCacheBytes(state)
			if GlobalRegistry != nil {
				GlobalRegistry.Lock.Lock()
				if cur, exists := GlobalRegistry.Files[state.Key]; exists && cur == state {
					delete(GlobalRegistry.Files, state.Key)
				}
				GlobalRegistry.Lock.Unlock()
			}
			log.Printf("%s Ephemeral TierTmp stream finalized and cleaned up: %s", logPrefix, state.Key)
		} else {
			log.Printf("%s Ephemeral TierTmp stream completed (%s); keeping temp file until active clients finish", logPrefix, state.Key)
		}

		runtime.GC()
		debug.FreeOSMemory()
		return
	}

	if tier == TierS3 {
		// Release the prehydration chain before S3 recomposition when called from coordinator completion
		// so the next queued episode can start downloading immediately instead of waiting for Recompose.
		if earlyPrehydrateRelease && GlobalPrehydrationManager != nil {
			GlobalPrehydrationManager.OnHydrationCompleted(state)
		}

		commitTimeout := 30 * time.Minute
		if GlobalConfig != nil {
			if rt := GlobalConfig.GetS3Config().RecomposeTimeout; rt > 0 {
				commitTimeout = rt
			}
		}
		commitCtx, commitCancel := context.WithTimeout(context.Background(), commitTimeout)
		defer commitCancel()

		var commitErr error
		cm := state.GetChunkManager()
		if cm != nil {
			commitErr = cm.WaitAndRecompose(commitCtx)
		} else if GlobalStorage != nil {
			commitErr = GlobalStorage.CommitLocalFile(commitCtx, state.Key, state.PartPath, meta)
		}

		_ = os.Remove(state.PartPath)
		if state.MetaPath != "" {
			_ = os.Remove(state.MetaPath)
		}
		releaseAccountedCacheBytes(state)

		if GlobalRegistry != nil {
			GlobalRegistry.MarkConnectionsAsInCache(state.Key)
		}
		if commitErr != nil {
			log.Printf("%s S3 commit/recomposition FAILED for %s: %v", logPrefix, state.Key, commitErr)
			canPreserveChunks := cm != nil && cm.IsMultipart() && cm.AllChunksUploaded()
			if canPreserveChunks {
				if idx := getGlobalCacheIndex(); idx != nil {
					idx.RecordPendingRecompose(state.Key, meta, cm.ChunkSize(), cm.TotalParts(), true)
					state.FlushTransferToIndex(idx)
				}
				state.Lock.Lock()
				hasClients := len(state.Clients) > 0
				state.UpstreamStatus = "Pending Recompose"
				if hasClients {
					state.IsCommitting = false
					state.WriterActive = false
				}
				state.Lock.Unlock()
				if GlobalLogBroker != nil {
					go GlobalLogBroker.BroadcastStateUpdate()
				}
				if hasClients {
					log.Printf("%s Preserving S3 chunks and active reader access for %s while recompose awaits retry", logPrefix, state.Key)
					runtime.GC()
					debug.FreeOSMemory()
					return
				}
				cm.Close()
			} else {
				if cm != nil {
					cm.Close()
					if IsS3Healthy() {
						cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 30*time.Second)
						_ = cm.Cleanup(cleanCtx)
						cleanCancel()
					}
				}
				state.SetUpstreamStatus("Error")
			}
		} else {
			state.SetUpstreamStatus("Completed")
			log.Printf("%s Cached file finalized and committed to S3: %s", logPrefix, state.Key)
			if idx := getGlobalCacheIndex(); idx != nil {
				idx.RecordCommitForTier(state.Key, meta, IndexTierS3)
				state.FlushTransferToIndex(idx)
			}
		}

		if !earlyPrehydrateRelease && commitErr == nil && GlobalPrehydrationManager != nil {
			GlobalPrehydrationManager.OnHydrationCompleted(state)
		}
		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}

		if GlobalRegistry != nil {
			GlobalRegistry.Lock.Lock()
			if cur, exists := GlobalRegistry.Files[state.Key]; exists && cur == state {
				delete(GlobalRegistry.Files, state.Key)
			}
			GlobalRegistry.Lock.Unlock()
		}

		runtime.GC()
		debug.FreeOSMemory()
		return
	}

	// Cross-FS NAS staging path (TierDisk + ScratchChunkManager)
	if cm := state.GetChunkManager(); cm != nil {
		recomposeTimeout := 30 * time.Minute
		if GlobalConfig != nil {
			if rt := GlobalConfig.GetS3Config().RecomposeTimeout; rt > 0 {
				recomposeTimeout = rt
			}
		}
		recomposeCtx, cancelRecompose := context.WithTimeout(context.Background(), recomposeTimeout)
		commitErr := cm.WaitAndRecompose(recomposeCtx)
		cancelRecompose()
		cm.Close()
		_ = os.Remove(state.PartPath)

		if commitErr != nil {
			log.Printf("%s NAS staging commit FAILED for %s: %v", logPrefix, state.Key, commitErr)
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
			_ = cm.Cleanup(cleanupCtx)
			cancelCleanup()
			releaseAccountedCacheBytes(state)
			state.SetUpstreamStatus("Error")
		} else {
			if GlobalRegistry != nil {
				GlobalRegistry.MarkConnectionsAsInCache(state.Key)
			}
			state.SetUpstreamStatus("Completed")
			log.Printf("%s Cached file finalized and committed to NAS cache: %s", logPrefix, state.Key)
		}

		if commitErr == nil && GlobalPrehydrationManager != nil {
			GlobalPrehydrationManager.OnHydrationCompleted(state)
		}
		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}

		if GlobalRegistry != nil {
			GlobalRegistry.Lock.Lock()
			if cur, exists := GlobalRegistry.Files[state.Key]; exists && cur == state {
				delete(GlobalRegistry.Files, state.Key)
			}
			GlobalRegistry.Lock.Unlock()
		}

		if commitErr == nil {
			recordLocalCommitToIndexAndDemoter(state, meta)
		}

		runtime.GC()
		debug.FreeOSMemory()
		return
	}

	renameOK := false
	if !FileExists(state.Path) {
		err := moveFileCrossFS(state.PartPath, state.Path)
		if err == nil {
			renameOK = true
			if cf, ferr := os.Open(state.Path); ferr == nil {
				FadviseDontNeed(cf.Fd(), 0, 0)
				_ = cf.Close()
			}

			metaPath := state.Path + ".json"
			if newBytes, merr := json.Marshal(meta); merr == nil {
				_ = writeMetaFileAtomic(metaPath, newBytes)
			}
			if GlobalRegistry != nil {
				GlobalRegistry.MarkConnectionsAsInCache(state.Key)
			}
			state.SetUpstreamStatus("Completed")

			if GlobalRegistry != nil {
				GlobalRegistry.Lock.Lock()
				if cur, exists := GlobalRegistry.Files[state.Key]; exists && cur == state {
					delete(GlobalRegistry.Files, state.Key)
				}
				GlobalRegistry.Lock.Unlock()
			}

			recordLocalCommitToIndexAndDemoter(state, meta)

			if GlobalLogBroker != nil {
				go GlobalLogBroker.BroadcastStateUpdate()
			}
			log.Printf("%s Cached file finalized and committed to disk: %s", logPrefix, state.Key)
		} else {
			log.Printf("%s Commit rename failed (deferring finalization): %v", logPrefix, err)
			if !earlyPrehydrateRelease {
				return
			}
			if GlobalRegistry != nil {
				GlobalRegistry.MarkConnectionsAsInCache(state.Key)
			}
			state.SetUpstreamStatus("Completed")
		}
	} else {
		if cf, ferr := os.Open(state.Path); ferr == nil {
			FadviseDontNeed(cf.Fd(), 0, 0)
			_ = cf.Close()
		}
		if GlobalRegistry != nil {
			GlobalRegistry.MarkConnectionsAsInCache(state.Key)
		}
		state.SetUpstreamStatus("Completed")
		if GlobalRegistry != nil {
			GlobalRegistry.Lock.Lock()
			if cur, exists := GlobalRegistry.Files[state.Key]; exists && cur == state {
				delete(GlobalRegistry.Files, state.Key)
			}
			GlobalRegistry.Lock.Unlock()
		}
		recordLocalCommitToIndexAndDemoter(state, meta)
		if GlobalLogBroker != nil {
			go GlobalLogBroker.BroadcastStateUpdate()
		}
	}

	if (renameOK || earlyPrehydrateRelease || FileExists(state.Path)) && GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.OnHydrationCompleted(state)
	}

	runtime.GC()
	debug.FreeOSMemory()
}

func recordLocalCommitToIndexAndDemoter(state *FileCacheState, meta FileMeta) {
	if state == nil {
		return
	}
	if idx := getGlobalCacheIndex(); idx != nil {
		idx.RecordCommitForTier(state.Key, meta, IndexTierDisk)
		state.FlushTransferToIndex(idx)
	}
	if demoter := getGlobalHybridDemoter(); demoter != nil {
		demoter.EnqueueEagerBackup(state.Key)
		demoter.TriggerWatermarkDemotion()
	}
}

