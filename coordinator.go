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
	state.WriterActive = false
	partPath := state.PartPath
	metaPath := state.MetaPath
	key := state.Key
	state.Lock.Unlock()

	if GlobalLogBroker != nil {
		GlobalLogBroker.BroadcastStateUpdate()
	}

	if cancel != nil {
		cancel()
	}

	if fileToClose != nil {
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
}

type Worker struct {
	id                int
	startOffset       int64
	cancel            context.CancelFunc
	currentOffset     int64 // guarded atomically
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

	state.Lock.Lock()
	if state.File != nil && state.File != file {
		_ = state.File.Close()
	}
	state.File = file
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
			state.Lock.Unlock()
			state.SetUpstreamStatus("Committing")
			log.Printf("[Coord] Committing completed cache file to disk: %s", state.Key)

			state.Lock.Lock()
			fileToClose := state.File
			state.File = nil
			state.Lock.Unlock()

			if fileToClose != nil {
				_ = fileToClose.Close()
			}

			if !FileExists(state.Path) {
				err := os.Rename(state.PartPath, state.Path)
				if err == nil {
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
				GlobalRegistry.MarkConnectionsAsInCache(state.Key)
				state.SetUpstreamStatus("Completed")
				GlobalRegistry.Lock.Lock()
				delete(GlobalRegistry.Files, state.Key)
				GlobalRegistry.Lock.Unlock()
				if GlobalLogBroker != nil {
					GlobalLogBroker.BroadcastStateUpdate()
				}
			}
		} else {
			var fileToClose *os.File
			shouldCleanUpIncomplete := len(state.Clients) == 0
			if shouldCleanUpIncomplete {
				fileToClose = state.File
				state.File = nil
			}
			state.Lock.Unlock()

			if fileToClose != nil {
				_ = fileToClose.Close()
			}

			if shouldCleanUpIncomplete {
				GlobalRegistry.Lock.Lock()
				existingState, exists := GlobalRegistry.Files[state.Key]
				if exists && existingState == state {
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

		state.Lock.Lock()
		state.WriterActive = false
		state.Lock.Unlock()

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
			spawnTimeNano:    time.Now().UnixNano(),
			lastByteTimeNano: time.Now().UnixNano(),
		}
		state.ActiveWorkers = append(state.ActiveWorkers, w)
		go runDownloadWorker(wCtx, w, state, initialHeader, file, removeWorker, parentCancel)
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
			isPrefetchHelper: true,
			spawnTimeNano:    time.Now().UnixNano(),
			lastByteTimeNano: time.Now().UnixNano(),
		}
		atomic.StoreInt32(&w.isPrefetching, 1)
		state.ActiveWorkers = append(state.ActiveWorkers, w)
		go runDownloadWorker(wCtx, w, state, initialHeader, file, removeWorker, parentCancel)
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
				suppressed := false
				state.WorkersLock.Lock()
				for _, w := range state.ActiveWorkers {
					wCurrent := atomic.LoadInt64(&w.currentOffset)

					lastByteNano := atomic.LoadInt64(&w.lastByteTimeNano)
					var workerStallDuration time.Duration
					if lastByteNano > 0 {
						workerStallDuration = time.Since(time.Unix(0, lastByteNano))
					} else if w.spawnTimeNano > 0 {
						workerStallDuration = time.Since(time.Unix(0, w.spawnTimeNano))
					}

					isWorkerStalled := stallTimeout > 0 && workerStallDuration >= stallTimeout

					// If a worker is already dedicated to this starving offset and has not stalled, suppress
					if w.startOffset == sb && !isWorkerStalled {
						suppressed = true
						break
					}

					if isWorkerStalled {
						log.Printf("[Coord] Worker %d starved of incoming bytes for %s (>= %s threshold) with client starving at %d. Breaking out of suppression.",
							w.id, workerStallDuration.Round(time.Millisecond), stallTimeout, sb)
						continue
					}

					// 1. If worker is streaming within 512KB ahead/behind
					if wCurrent <= sb && sb-wCurrent <= 512*1024 {
						log.Printf("[Coord] Client Hydration Wait active for offset %d (worker %d is at %d). Suppressing jump.", sb, w.id, wCurrent)
						suppressed = true
						break
					}

					// If worker is prefetching tail metadata and target is within tail range
					if atomic.LoadInt32(&w.isPrefetching) == 1 && sb >= wCurrent {
						log.Printf("[Coord] Worker %d is pre-fetching tail metadata (at %d, client starving at %d). Suppressing pre-emption.", w.id, wCurrent, sb)
						suppressed = true
						break
					}

					// 2. Sequential catch-up ETA calculation
					if wCurrent < sb {
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
									log.Printf("[Coord] Client Hydration Wait active for offset %d (ETA %.2fs < threshold %s). Suppressing connection jump.", sb, eta.Seconds(), hydrationWait)
									suppressed = true
									break
								}
							}
						}
					}
				}
				state.WorkersLock.Unlock()

				if suppressed {
					continue
				}

				state.WorkersLock.Lock()
				activeCount = len(state.ActiveWorkers)
				state.WorkersLock.Unlock()

				maxWorkers := GlobalConfig.GetMaxUpstreamWorkers()
				if activeCount < maxWorkers {
					log.Printf("[Coord] Spawning new worker starting at starving offset %d (Active: %d/%d)", sb, activeCount+1, maxWorkers)
					spawnWorker(sb)
				} else {
					// Worker limit reached. Find furthest worker to pre-empt
					state.WorkersLock.Lock()
					var workerToPreempt *Worker
					maxDistance := int64(-1)

					for _, w := range state.ActiveWorkers {
						wCurrent := atomic.LoadInt64(&w.currentOffset)

						connStartNano := atomic.LoadInt64(&w.connStartTimeNano)
						var elapsed time.Duration
						if connStartNano > 0 {
							elapsed = time.Since(time.Unix(0, connStartNano))
						} else if w.spawnTimeNano > 0 {
							elapsed = time.Since(time.Unix(0, w.spawnTimeNano))
						}

						lastByteNano := atomic.LoadInt64(&w.lastByteTimeNano)
						var workerStallDuration time.Duration
						if lastByteNano > 0 {
							workerStallDuration = time.Since(time.Unix(0, lastByteNano))
						} else if w.spawnTimeNano > 0 {
							workerStallDuration = time.Since(time.Unix(0, w.spawnTimeNano))
						}
						isWorkerStalled := stallTimeout > 0 && workerStallDuration >= stallTimeout

						minTime := GlobalConfig.GetSchedulerMinTime()
						eligible := minTime <= 0 || elapsed >= minTime || isWorkerStalled

						if eligible && atomic.LoadInt32(&w.isPrefetching) == 0 {
							var minDistanceToClient int64 = 1e18
							state.Lock.Lock()
							for _, clientStarveOffset := range state.Starving {
								dist := clientStarveOffset - wCurrent
								if dist < 0 {
									dist = -dist
								}
								if dist < minDistanceToClient {
									minDistanceToClient = dist
								}
							}
							state.Lock.Unlock()
							if minDistanceToClient > maxDistance {
								maxDistance = minDistanceToClient
								workerToPreempt = w
							}
						}
					}

					if workerToPreempt != nil {
						log.Printf("[Coord] Pre-empting worker %d (at offset %d) to switch to starving offset %d",
							workerToPreempt.id, atomic.LoadInt64(&workerToPreempt.currentOffset), sb)
						workerToPreempt.cancel()

						for i, aw := range state.ActiveWorkers {
							if aw.id == workerToPreempt.id {
								state.ActiveWorkers = append(state.ActiveWorkers[:i], state.ActiveWorkers[i+1:]...)
								break
							}
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
					} else {
						state.WorkersLock.Unlock()
					}
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

	// Pre-flight check: was this start offset already satisfied by another worker during connect?
	if state.RangeSet.Contains(currentOffset) && !w.isPrefetchHelper {
		gapStart, _, found := state.RangeSet.FindNextGap(currentOffset, state.TotalSize)
		if !found {
			log.Printf("[Worker-%d] Start offset %d already satisfied and file complete. Exiting.", w.id, currentOffset)
			return
		}
		currentOffset = gapStart
		atomic.StoreInt64(&w.currentOffset, currentOffset)
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
			runInlineTailPrefetch(ctx, w, state, initialHeader, file, pendingTail)
			continue
		}

		// Dynamically compute next boundary (next cached chunk or downstream worker start)
		boundary := state.GetNextBoundary(w.id, currentOffset)

		// Check if current offset is already covered in RangeSet or past boundary
		if boundary <= currentOffset || state.RangeSet.Contains(currentOffset) {
			gapStart, _, found := state.RangeSet.FindNextGap(currentOffset, state.TotalSize)
			if !found {
				log.Printf("[Worker-%d] Range from %d to EOF completed. Worker coalescing and exiting.", w.id, currentOffset)
				closeConnection()
				return
			}
			log.Printf("[Worker-%d] Reached completed range or boundary at %d. Leapfrogging to next gap at %d", w.id, currentOffset, gapStart)
			closeConnection()
			currentOffset = gapStart
			atomic.StoreInt64(&w.currentOffset, currentOffset)
			boundary = state.GetNextBoundary(w.id, currentOffset)
			if boundary <= currentOffset {
				return
			}
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

			rangeHeader := fmt.Sprintf("bytes=%d-%d", currentOffset, boundary-1)
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
				gapStart, _, found := state.RangeSet.FindNextGap(currentOffset, state.TotalSize)
				if !found {
					return
				}
				currentOffset = gapStart
				atomic.StoreInt64(&w.currentOffset, currentOffset)
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
			_, werr := file.WriteAt(buf[:n], currentOffset)
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

			// In-flight collision & boundary check: check if worker reached downstream boundary or cached range
			curBoundary := state.GetNextBoundary(w.id, currentOffset)
			if currentOffset >= curBoundary || state.RangeSet.Contains(currentOffset) {
				log.Printf("[Worker-%d] Reached downstream boundary or cached data at offset %d (target: %d). Leapfrogging to next gap...", w.id, currentOffset, curBoundary)
				closeConnection()
				gapStart, _, found := state.RangeSet.FindNextGap(currentOffset, state.TotalSize)
				if !found {
					log.Printf("[Worker-%d] All remaining gaps completed. Worker coalescing and exiting.", w.id)
					return
				}
				currentOffset = gapStart
				atomic.StoreInt64(&w.currentOffset, currentOffset)
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

			if handleFailure(false, "[Worker-%d] Upstream read error at %d: %v", w.id, currentOffset, err) {
				return
			}
			continue
		}
	}
}

func (s *FileCacheState) GetNextBoundary(workerID int, currentOffset int64) int64 {
	boundary := s.TotalSize

	// 1. Next cached chunk boundary from RangeSet
	gapStart, gapEnd, found := s.RangeSet.FindNextGap(currentOffset, s.TotalSize)
	if found && gapStart == currentOffset && gapEnd > currentOffset {
		boundary = gapEnd
	} else if found && gapStart > currentOffset {
		boundary = gapStart
	}

	// 2. Next downstream active worker position
	s.WorkersLock.Lock()
	for _, other := range s.ActiveWorkers {
		if other.id != workerID {
			otherCurrent := atomic.LoadInt64(&other.currentOffset)
			if otherCurrent > currentOffset && otherCurrent < boundary {
				boundary = otherCurrent
			}
			if other.startOffset > currentOffset && other.startOffset < boundary {
				boundary = other.startOffset
			}
		}
	}
	s.WorkersLock.Unlock()

	return boundary
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
			_, werr := file.WriteAt(buf[:n], currentOffset)
			if werr != nil {
				log.Printf("[Worker-%d] Inline tail pre-fetch write error at %d: %v", w.id, currentOffset, werr)
				break
			}

			state.RangeSet.Add(currentOffset, currentOffset+int64(n))
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
	n, err := file.ReadAt(buf, 0)
	if (err == nil || err == io.EOF) && n > 0 {
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
