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
	coordinatorSleepFn = time.Sleep
)

// StartCoordinator spawns the manager goroutine for a given file
func StartCoordinator(state *FileCacheState, initialHeader http.Header) {
	state.Lock.Lock()
	if state.WriterActive {
		state.Lock.Unlock()
		return
	}
	state.WriterActive = true
	state.Lock.Unlock()

	if GlobalLogBroker != nil {
		GlobalLogBroker.BroadcastStateUpdate()
	}

	go runCoordinator(state, initialHeader)
}

// CancelCoordinator cancels an active coordinator, stopping workers and deleting partial files if no clients exist
func CancelCoordinator(state *FileCacheState) {
	state.Lock.Lock()
	cancel := state.Cancel
	state.Cancel = nil
	state.UpstreamStatus = "Cancelled"
	clientsCount := len(state.Clients)
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

	if GlobalLogBroker != nil {
		GlobalLogBroker.BroadcastStateUpdate()
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
			if FileExists(partPath) {
				_ = os.Remove(partPath)
			}
			if FileExists(metaPath) {
				_ = os.Remove(metaPath)
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
		GlobalLogBroker.BroadcastStateUpdate()
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

func runCoordinator(state *FileCacheState, initialHeader http.Header) {
	log.Printf("[Coord] Starting concurrent background coordinator for cache: %s", state.Key)

	// Open the target file in Read/Write mode (or create it)
	// Truncate to allocate file space on disk immediately
	var file *os.File
	var err error
	if FileExists(state.PartPath) {
		file, err = os.OpenFile(state.PartPath, os.O_RDWR, 0644)
	} else {
		file, err = os.OpenFile(state.PartPath, os.O_CREATE|os.O_RDWR, 0644)
	}
	if err != nil {
		log.Printf("[Coord] Error opening part file: %v", err)
		state.Lock.Lock()
		state.WriterActive = false
		state.Lock.Unlock()
		return
	}
	_ = file.Truncate(state.TotalSize)

	pipeline := NewWritebackPipeline(file, state, GlobalConfig.GetWritebackBufferSize())

	state.Lock.Lock()
	if state.File != nil && state.File != file {
		_ = state.File.Close()
	}
	state.File = file
	state.Pipeline = pipeline
	if state.HydrationStartTime == 0 {
		state.HydrationStartTime = time.Now().Unix()
	}
	state.Lock.Unlock()

	parentCtx, parentCancel := context.WithCancel(context.Background())
	state.Lock.Lock()
	state.Cancel = parentCancel
	state.Lock.Unlock()

	workerIdGen := 0

	defer func() {
		parentCancel()
		state.Lock.Lock()
		state.Cancel = nil

		// Wake up any client readers waiting on data
		for _, ch := range state.Clients {
			safeSignal(ch)
		}

		isCompleted := state.RangeSet.IsComplete(state.TotalSize)

		if isCompleted {
			fileToClose := state.File
			state.File = nil
			pipelineToClose := state.Pipeline
			state.Pipeline = nil
			state.Lock.Unlock()

			state.SetUpstreamStatus("Committing")
			log.Printf("[Coord] Committing completed cache file to disk: %s", state.Key)

			go finalizeAndCommitCache(state, fileToClose, pipelineToClose)
		} else {
			var fileToClose *os.File
			var pipelineToClose StorageSink
			shouldCleanUpIncomplete := len(state.Clients) == 0
			if shouldCleanUpIncomplete {
				fileToClose = state.File
				state.File = nil
				pipelineToClose = state.Pipeline
				state.Pipeline = nil
			}
			state.WriterActive = false
			state.Lock.Unlock()

			if pipelineToClose != nil {
				_ = pipelineToClose.FlushAndClose()
			}
			if fileToClose != nil {
				_ = fileToClose.Close()
			}

			if shouldCleanUpIncomplete {
				GlobalRegistry.Lock.Lock()
				existingState, exists := GlobalRegistry.Files[state.Key]
				if exists && (existingState == state || existingState == nil) {
					delete(GlobalRegistry.Files, state.Key)
					GlobalRegistry.Lock.Unlock()
					_ = os.Remove(state.PartPath)
					_ = os.Remove(state.MetaPath)
					log.Printf("[Coord] Removed incomplete cache file from registry and disk: %s", state.Key)
				} else {
					GlobalRegistry.Lock.Unlock()
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

	spawnWorker := func(startOffset int64) {
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
		go runDownloadWorker(wCtx, w, state, initialHeader, file, pipeline, removeWorker, parentCancel)
	}

	spawnPrefetchWorker := func(startOffset int64) {
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
					log.Printf("[Coord] Immediately clamped Worker %d target boundary to %d (metadata prefetch Worker %d)", other.id, startOffset, w.id)
				}
			}
		}

		state.ActiveWorkers = append(state.ActiveWorkers, w)
		go runDownloadWorker(wCtx, w, state, initialHeader, file, pipeline, removeWorker, parentCancel)
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
	if initialTargetOffset == -1 {
		gapStart, _, found := state.RangeSet.FindNextGap(0, state.TotalSize)
		if found {
			initialTargetOffset = gapStart
		}
	}
	state.Lock.Unlock()

	if initialTargetOffset != -1 {
		spawnWorker(initialTargetOffset)
	}

	// Main Coordinator Orchestration Loop
	for {
		// Sniff check for container optimization
		sniffAndSchedulePrefetch(state, file)

		// Check if fully completed
		if state.RangeSet.IsComplete(state.TotalSize) {
			break
		}

		// Keep-alive check: if no workers are running, start one at first gap
		state.WorkersLock.Lock()
		activeCount := len(state.ActiveWorkers)
		state.WorkersLock.Unlock()

		if activeCount == 0 {
			gapStart, _, found := state.RangeSet.FindNextGap(0, state.TotalSize)
			if found {
				log.Printf("[Coord] No active workers but hydration incomplete. Spawning worker at offset %d", gapStart)
				spawnWorker(gapStart)
			}
		}

		select {
		case <-state.NotifyChan:
		case <-time.After(500 * time.Millisecond):
		case <-parentCtx.Done():
			return
		}

		state.Lock.Lock()
		pendingTail := state.PendingTailPrefetchOffset
		if pendingTail > 0 && GlobalConfig.GetMaxUpstreamWorkers() > 1 {
			state.PendingTailPrefetchOffset = -1
			state.Lock.Unlock()
			log.Printf("[Coord] Sniffed non-optimized container. Spawning parallel helper worker to pre-fetch metadata starting at offset %d", pendingTail)
			spawnPrefetchWorker(pendingTail)
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
					log.Printf("[Coord] Worker %d starved of incoming bytes for %s (>= %s threshold) with client starving at %d. Breaking out of suppression and reconnecting.",
						primaryStalled.id, stallDuration.Round(time.Millisecond), stallTimeout, sb)

					for _, sw := range stalledWorkers {
						sw.cancel()
						removeWorkerLocked(sw.id)
					}
					state.WorkersLock.Unlock()

					state.Lock.Lock()
					for clientID, starveOff := range state.Starving {
						if starveOff == sb {
							state.StarvingSince[clientID] = time.Now()
						}
					}
					state.Lock.Unlock()

					spawnWorker(sb)
					continue
				}

				// Case 3: No worker covers sb (player seek or uncovered gap)
				activeCount = len(state.ActiveWorkers)
				maxWorkers := GlobalConfig.GetMaxUpstreamWorkers()
				if activeCount < maxWorkers {
					state.WorkersLock.Unlock()
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

					spawnWorker(sb)
				} else {
					state.WorkersLock.Unlock()
				}
			}
		}
	}
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
				isPermanent := resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone
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

		// Read streaming chunk from upstream HTTP body
		readStartTime := time.Now()
		n, err := activeRespBody.Read(buf)
		if n > 0 {
			var werr error
			if pipeline != nil {
				werr = pipeline.WriteChunk(currentOffset, buf[:n])
			} else if file != nil {
				_, werr = file.WriteAt(buf[:n], currentOffset)
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
			state.RangeSet.Add(currentOffset, currentOffset+int64(n))
			if state.ChunkManager != nil {
				state.ChunkManager.CheckAndTriggerUploads()
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

			atomic.AddInt64(&state.BytesDownloadedSec, int64(n))
			atomic.AddInt64(&state.TotalBytesDownloaded, int64(n))
			GlobalRegistry.UpdateSize(int64(n))

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

	// 2. Search forward from fromOffset to TotalSize
	curr := fromOffset
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

	// 3. Wrap around from 0 to fromOffset for unfulfilled backfill gaps
	if fromOffset > 0 {
		curr = 0
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
			var werr error
			if pipeline != nil {
				werr = pipeline.WriteChunk(currentOffset, buf[:n])
			} else if file != nil {
				_, werr = file.WriteAt(buf[:n], currentOffset)
			}
			if werr != nil {
				log.Printf("[Worker-%d] Inline tail pre-fetch write error at %d: %v", w.id, currentOffset, werr)
				break
			}

			state.RangeSet.Add(currentOffset, currentOffset+int64(n))
			if state.ChunkManager != nil {
				state.ChunkManager.CheckAndTriggerUploads()
			}
			state.Lock.Lock()
			for _, ch := range state.Clients {
				safeSignal(ch)
			}
			state.Lock.Unlock()

			currentOffset += int64(n)
			atomic.StoreInt64(&w.currentOffset, currentOffset)

			atomic.AddInt64(&state.BytesDownloadedSec, int64(n))
			atomic.AddInt64(&state.TotalBytesDownloaded, int64(n))
			GlobalRegistry.UpdateSize(int64(n))
		}

		if err != nil {
			break
		}
	}

	log.Printf("[Worker-%d] Inline tail pre-fetch finished. Resuming sequential stream.", w.id)
}

func sniffAndSchedulePrefetch(state *FileCacheState, file *os.File) {
	state.Lock.Lock()
	if state.Sniffed {
		state.Lock.Unlock()
		return
	}

	avail := state.RangeSet.AvailableFrom(0)
	isHeaderReady := avail >= 128*1024 || (state.TotalSize > 0 && avail >= state.TotalSize)
	if !isHeaderReady {
		state.Lock.Unlock()
		return
	}
	state.Sniffed = true
	state.Lock.Unlock()

	log.Printf("[Coord] Initial header bytes available. Sniffing media container...")
	buf := make([]byte, 128*1024)
	n := 0
	if state.LiveBuffer != nil {
		n, _ = state.LiveBuffer.ReadAt(buf, 0)
	}
	if n == 0 && file != nil {
		n, _ = file.ReadAt(buf, 0)
	}
	if n > 0 {
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

func finalizeAndCommitCache(state *FileCacheState, fileToClose *os.File, pipeline StorageSink) {
	defer func() {
		state.Lock.Lock()
		state.WriterActive = false
		state.Lock.Unlock()
	}()

	if pipeline != nil {
		if err := pipeline.FlushAndClose(); err != nil {
			log.Printf("[Coord] Error flushing writeback pipeline during commit: %v", err)
		}
	}

	if fileToClose != nil {
		FadviseDontNeed(fileToClose.Fd(), 0, 0)
		if err := fileToClose.Close(); err != nil {
			log.Printf("[Coord] Error closing part file during commit: %v", err)
		}
	}

	state.Lock.Lock()
	if state.LiveBuffer != nil {
		state.LiveBuffer.Clear()
		state.LiveBuffer = nil
	}
	state.Lock.Unlock()

	if GlobalStorage != nil && GlobalStorage.Type() == "s3" {
		meta := FileMeta{
			URL:           state.URL,
			TotalSize:     state.TotalSize,
			ContentType:   state.ContentType,
			ETag:          state.ETag,
			LastModified:  state.LastModified,
			LastValidated: time.Now().Unix(),
			Completed:     true,
		}

		if state.ChunkManager != nil {
			state.ChunkManager.AttemptRecompose()
		} else {
			_ = GlobalStorage.CommitLocalFile(context.Background(), state.Key, state.PartPath, meta)
		}

		_ = os.Remove(state.PartPath)
		_ = os.Remove(state.MetaPath)

		GlobalRegistry.MarkConnectionsAsInCache(state.Key)
		state.SetUpstreamStatus("Completed")

		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, state.Key)
		GlobalRegistry.Lock.Unlock()

		if GlobalLogBroker != nil {
			GlobalLogBroker.BroadcastStateUpdate()
		}
		log.Printf("[Coord] Cached file finalized and committed to S3: %s", state.Key)
		return
	}

	if !FileExists(state.Path) {
		err := os.Rename(state.PartPath, state.Path)
		if err == nil {
			// Evict the committed file from OS page cache
			if cf, ferr := os.Open(state.Path); ferr == nil {
				FadviseDontNeed(cf.Fd(), 0, 0)
				_ = cf.Close()
			}

			// Save final metadata JSON confirming completion
			metaPath := state.Path + ".json"
			meta := FileMeta{
				URL:           state.URL,
				TotalSize:     state.TotalSize,
				ContentType:   state.ContentType,
				ETag:          state.ETag,
				LastModified:  state.LastModified,
				LastValidated: time.Now().Unix(),
				Completed:     true,
			}
			newBytes, err := json.Marshal(meta)
			if err == nil {
				_ = os.WriteFile(metaPath, newBytes, 0644)
			}
			GlobalRegistry.MarkConnectionsAsInCache(state.Key)
			state.SetUpstreamStatus("Completed")

			GlobalRegistry.Lock.Lock()
			delete(GlobalRegistry.Files, state.Key)
			GlobalRegistry.Lock.Unlock()

			if GlobalLogBroker != nil {
				GlobalLogBroker.BroadcastStateUpdate()
			}
			log.Printf("[Coord] Cached file finalized and committed to disk: %s", state.Key)
		} else {
			log.Printf("[Coord] Commit rename failed (likely Windows lock, deferring to reader exit): %v", err)
			GlobalRegistry.MarkConnectionsAsInCache(state.Key)
			state.SetUpstreamStatus("Completed")
		}
	} else {
		// Evict existing file from OS page cache
		if cf, ferr := os.Open(state.Path); ferr == nil {
			FadviseDontNeed(cf.Fd(), 0, 0)
			_ = cf.Close()
		}
		GlobalRegistry.MarkConnectionsAsInCache(state.Key)
		state.SetUpstreamStatus("Completed")
		GlobalRegistry.Lock.Lock()
		delete(GlobalRegistry.Files, state.Key)
		GlobalRegistry.Lock.Unlock()
		if GlobalLogBroker != nil {
			GlobalLogBroker.BroadcastStateUpdate()
		}
	}

	runtime.GC()
	debug.FreeOSMemory()
}

