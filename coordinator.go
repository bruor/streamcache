package main

import (
	"context"
	"encoding/json"
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

	go runCoordinator(state, initialHeader)
}

type Worker struct {
	id                int
	startBlock        int
	cancel            context.CancelFunc
	currentBlock      int
	offset            int64 // guarded atomically
	speed             int64 // estimated speed (bytes/sec)
	connStartTimeNano int64 // Unix nano timestamp of connection start, guarded atomically
	connBytesWritten   int64 // bytes read on this connection, guarded atomically
	isPrefetching     int32 // 1 if prefetching last blocks (metadata), 0 otherwise, guarded atomically
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
	state.File = file
	state.Lock.Unlock()

	parentCtx, parentCancel := context.WithCancel(context.Background())
	workerIdGen := 0

	defer func() {
		parentCancel()
		state.Lock.Lock()

		// We check if all blocks are completed to ensure we only finalize fully completed files.
		isCompleted := len(state.BlockStates) > 0
		for _, status := range state.BlockStates {
			if status != BlockCompleted {
				isCompleted = false
				break
			}
		}

		if isCompleted {
			state.UpstreamStatus = "Committing"
			state.Lock.Unlock()
			if file != nil {
				log.Printf("[Coord] Committing and flushing cache file to disk: %s", state.Key)
				_ = file.Sync()
			}
			state.Lock.Lock()
			state.UpstreamStatus = "Completed"
			GlobalRegistry.MarkConnectionsAsInCache(state.Key)
		} else {
			state.UpstreamStatus = "Idle"
		}

		if isCompleted {
			// Try to finalize immediately on completion
			var immediateFinalize bool
			if !FileExists(state.Path) {
				// Unlock before performing OS operations and lock acquisitions
				state.Lock.Unlock()
				err := os.Rename(state.PartPath, state.Path)
				if err == nil {
					immediateFinalize = true
					// Save final metadata JSON confirming completion
					metaPath := state.Path + ".json"
					meta := FileMeta{
						URL:           state.URL,
						TotalSize:     state.TotalSize,
						ETag:          state.ETag,
						LastModified:  state.LastModified,
						LastValidated: time.Now().Unix(),
						Completed:     true,
					}
					newBytes, err := json.Marshal(meta)
					if err == nil {
						_ = os.WriteFile(metaPath, newBytes, 0644)
					}
					GlobalRegistry.Lock.Lock()
					delete(GlobalRegistry.Files, state.Key)
					GlobalRegistry.Lock.Unlock()
					log.Printf("[Coord] Cached file finalized immediately on coordinator completion: %s", state.Key)
				} else {
					log.Printf("[Coord] Immediate finalize rename failed (likely Windows lock, deferring): %v", err)
				}
				state.Lock.Lock() // Relock to finish the evaluation of deferred paths
			}

			var fileToClose *os.File
			shouldFinalize := !immediateFinalize && len(state.Clients) == 0 && state.File != nil && !FileExists(state.Path)
			if shouldFinalize {
				fileToClose = state.File
				state.File = nil
			}
			state.Lock.Unlock()

			if fileToClose != nil {
				_ = fileToClose.Close()
			}

			if shouldFinalize {
				err := os.Rename(state.PartPath, state.Path)
				if err == nil {
					// Save final metadata JSON confirming completion
					metaPath := state.Path + ".json"
					meta := FileMeta{
						URL:           state.URL,
						TotalSize:     state.TotalSize,
						ETag:          state.ETag,
						LastModified:  state.LastModified,
						LastValidated: time.Now().Unix(),
						Completed:     true,
					}
					newBytes, err := json.Marshal(meta)
					if err == nil {
						_ = os.WriteFile(metaPath, newBytes, 0644)
					}
					GlobalRegistry.Lock.Lock()
					delete(GlobalRegistry.Files, state.Key)
					GlobalRegistry.Lock.Unlock()
					log.Printf("[Coord] Cached file finalized on coordinator exit (deferred): %s", state.Key)
				} else {
					log.Printf("[Coord] Error renaming cache file on coordinator exit (deferred): %v", err)
				}
			}
		} else {
			// Incomplete download
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
				_ = os.Remove(state.PartPath)
				_ = os.Remove(state.MetaPath)
				GlobalRegistry.Lock.Lock()
				delete(GlobalRegistry.Files, state.Key)
				GlobalRegistry.Lock.Unlock()
				log.Printf("[Coord] Removed incomplete cache file from registry and disk: %s", state.Key)
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

	spawnWorker := func(startBlock int) {
		state.WorkersLock.Lock()
		defer state.WorkersLock.Unlock()
		workerIdGen++
		wCtx, wCancel := context.WithCancel(parentCtx)
		w := &Worker{
			id:           workerIdGen,
			startBlock:   startBlock,
			cancel:       wCancel,
			currentBlock: startBlock,
		}
		state.ActiveWorkers = append(state.ActiveWorkers, w)
		go runDownloadWorker(wCtx, w, state, initialHeader, file, removeWorker, parentCancel)
	}

	// Initial scan to start worker(s)
	state.Lock.Lock()
	initialTargetBlock := -1
	for _, starvingBlock := range state.Starving {
		if state.BlockStates[starvingBlock] != BlockCompleted {
			initialTargetBlock = starvingBlock
			break
		}
	}
	if initialTargetBlock == -1 {
		// Start at first uncompleted block
		for i, status := range state.BlockStates {
			if status != BlockCompleted {
				initialTargetBlock = i
				break
			}
		}
	}
	state.Lock.Unlock()

	if initialTargetBlock != -1 {
		spawnWorker(initialTargetBlock)
	}

	// Main Coordinator Orchestration Loop
	for {
		// Check if fully completed
		state.Lock.Lock()
		isCompleted := len(state.BlockStates) > 0
		for _, status := range state.BlockStates {
			if status != BlockCompleted {
				isCompleted = false
				break
			}
		}
		state.Lock.Unlock()
		
		if isCompleted {
			break
		}

		// Keep-alive check: if no workers are running, start one
		state.WorkersLock.Lock()
		activeCount := len(state.ActiveWorkers)
		state.WorkersLock.Unlock()
		
		if activeCount == 0 {
			state.Lock.Lock()
			nextUncompletedBlock := -1
			for i, status := range state.BlockStates {
				if status != BlockCompleted {
					nextUncompletedBlock = i
					break
				}
			}
			state.Lock.Unlock()
			
			if nextUncompletedBlock != -1 {
				log.Printf("[Coord] No active workers but hydration incomplete. Spawning worker at block %d", nextUncompletedBlock)
				spawnWorker(nextUncompletedBlock)
			}
		}

		select {
		case <-state.NotifyChan:
			state.Lock.Lock()
			
			// If any active worker is currently pre-fetching index/metadata,
			// suppress spawning of new workers and pre-emption to avoid race conditions.
			isPreFetchingActive := false
			state.WorkersLock.Lock()
			for _, w := range state.ActiveWorkers {
				if atomic.LoadInt32(&w.isPrefetching) == 1 {
					isPreFetchingActive = true
					break
				}
			}
			state.WorkersLock.Unlock()

			if isPreFetchingActive {
				state.Lock.Unlock()
				continue
			}

			// Find all starving blocks that are not completed or currently actively downloading
			var starvingBlocks []int
			for _, sb := range state.Starving {
				if state.BlockStates[sb] != BlockCompleted {
					// Check if any active worker is currently downloading this block
					isBeingDownloaded := false
					state.WorkersLock.Lock()
					for _, w := range state.ActiveWorkers {
						if w.currentBlock == sb {
							isBeingDownloaded = true
							break
						}
					}
					state.WorkersLock.Unlock()

					if !isBeingDownloaded {
						alreadyInList := false
						for _, b := range starvingBlocks {
							if b == sb {
								alreadyInList = true
								break
							}
						}
						if !alreadyInList {
							starvingBlocks = append(starvingBlocks, sb)
						}
					}
				}
			}
			
			if len(starvingBlocks) > 0 {
				// For each starvation target, decide whether to wait (Hydration Wait Time) or spawn/preempt
				for _, sb := range starvingBlocks {
					suppressed := false
					state.WorkersLock.Lock()
					for _, w := range state.ActiveWorkers {
						wCurrent := w.currentBlock
						if wCurrent < 0 {
							continue
						}

						// 1. If the worker is already downloading sb, or is about to download sb next
						if wCurrent == sb || wCurrent+1 == sb {
							log.Printf("[Coord] Client Hydration Wait active for block %d (worker %d is at block %d). Suppressing connection jump.", 
								sb, w.id, wCurrent)
							suppressed = true
							break
						}

						// 2. If the worker is further behind, check if it will reach sb sequentially
						if wCurrent < sb {
							// Check if there are any completed blocks in between
							hasCompletedBlockInPath := false
							for b := wCurrent + 1; b < sb; b++ {
								if state.BlockStates[b] == BlockCompleted {
									hasCompletedBlockInPath = true
									break
								}
							}

							if !hasCompletedBlockInPath {
								wOffset := atomic.LoadInt64(&w.offset)
								wSpeed := atomic.LoadInt64(&w.speed)

								// Estimate speed dynamically if not set yet
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
									targetOffset := int64(sb) * GlobalConfig.BlockSize
									if wOffset <= targetOffset {
										bytesRemaining := (int64(sb)+1)*GlobalConfig.BlockSize - wOffset
										eta := time.Duration(float64(bytesRemaining)/float64(wSpeed)) * time.Second
										if eta < GlobalConfig.ClientHydrationWait {
											log.Printf("[Coord] Client Hydration Wait active for block %d (ETA %.2fs < threshold %s). Suppressing connection jump.", 
												sb, eta.Seconds(), GlobalConfig.ClientHydrationWait)
											suppressed = true
											break
										}
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

					if activeCount < GlobalConfig.MaxUpstreamWorkers {
						log.Printf("[Coord] Spawning new worker starting at starving block %d (Active: %d/%d)", 
							sb, activeCount+1, GlobalConfig.MaxUpstreamWorkers)
						spawnWorker(sb)
					} else {
						// Worker limit reached. Find furthest worker to pre-empt
						state.WorkersLock.Lock()
						var workerToPreempt *Worker
						maxDistance := int64(-1)
						
						for _, w := range state.ActiveWorkers {
							wCurrent := w.currentBlock
							
							// Check debouncer always to protect CDN from connection thrashing
							connStartNano := atomic.LoadInt64(&w.connStartTimeNano)
							connBytes := atomic.LoadInt64(&w.connBytesWritten)
							var elapsed time.Duration
							if connStartNano > 0 {
								elapsed = time.Since(time.Unix(0, connStartNano))
							}
							
							eligible := false
							if GlobalConfig.SchedulerMinTime <= 0 && GlobalConfig.SchedulerMinBytes <= 0 {
								eligible = true
							} else {
								timeOk := GlobalConfig.SchedulerMinTime > 0 && elapsed >= GlobalConfig.SchedulerMinTime
								bytesOk := GlobalConfig.SchedulerMinBytes > 0 && connBytes >= GlobalConfig.SchedulerMinBytes
								eligible = timeOk || bytesOk
							}

							if eligible {
								var minDistanceToClient int64 = 1e18
								for _, clientStarveBlock := range state.Starving {
									dist := int64(clientStarveBlock - wCurrent)
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
						
						if workerToPreempt != nil {
							log.Printf("[Coord] Pre-empting worker %d (at block %d) to switch to starving block %d", 
								workerToPreempt.id, workerToPreempt.currentBlock, sb)
							workerToPreempt.cancel()
							
							// Remove from ActiveWorkers slice immediately to prevent reuse
							for i, aw := range state.ActiveWorkers {
								if aw.id == workerToPreempt.id {
									state.ActiveWorkers = append(state.ActiveWorkers[:i], state.ActiveWorkers[i+1:]...)
									break
								}
							}
							state.WorkersLock.Unlock()
							
							spawnWorker(sb)
						} else {
							state.WorkersLock.Unlock()
						}
					}
				}
			}
			state.Lock.Unlock()

		case <-parentCtx.Done():
			return
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
	log.Printf("[Worker-%d] Starting background downloader at block %d", w.id, w.startBlock)

	consecutiveFailures := 0
	backoff := 2 * time.Second
	currentBlock := w.startBlock
	
	handleFailure := func(isPermanent bool, errFormat string, args ...interface{}) bool {
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
		if consecutiveFailures >= 8 {
			log.Printf("[Worker-%d] Maximum consecutive failures reached (8). Exiting worker.", w.id)
			parentCancel()
			return true
		}
		
		log.Printf("[Worker-%d] Retry %d/8. Sleeping %s before retrying...", w.id, consecutiveFailures, backoff)
		
		state.Lock.Lock()
		state.UpstreamStatus = "Throttled"
		state.Lock.Unlock()

		coordinatorSleepMu.RLock()
		sleepFn := coordinatorSleepFn
		coordinatorSleepMu.RUnlock()
		sleepFn(backoff)

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

	blockSize := GlobalConfig.BlockSize
	targetStartOffset := state.TotalSize - GlobalConfig.SchedulerMinBytes
	if targetStartOffset < 0 {
		targetStartOffset = 0
	}
	firstEndBlock := int(targetStartOffset / blockSize)
	blocksToGrab := state.TotalBlocks - firstEndBlock

	if w.startBlock == 0 && state.TotalBlocks >= 10 && firstEndBlock > 0 && blocksToGrab > 0 {
		lastBlocksUncompleted := false
		state.Lock.Lock()
		for b := firstEndBlock; b < state.TotalBlocks; b++ {
			if state.BlockStates[b] != BlockCompleted {
				lastBlocksUncompleted = true
				break
			}
		}
		state.Lock.Unlock()

		if lastBlocksUncompleted {
			log.Printf("[Worker-%d] Pre-fetching last %d blocks for container index metadata sequentially...", w.id, blocksToGrab)
			atomic.StoreInt32(&w.isPrefetching, 1)

			prefetchStart := int64(firstEndBlock) * blockSize
			prefetchLength := state.TotalSize - prefetchStart

			req, err := http.NewRequestWithContext(ctx, "GET", state.URL, nil)
			if err == nil {
				rangeHeader := fmt.Sprintf("bytes=%d-%d", prefetchStart, state.TotalSize-1)
				req.Header.Set("Range", rangeHeader)
				if initialHeader != nil && len(initialHeader) > 0 {
					for k, vv := range initialHeader {
						if k != "Range" && k != "Connection" && k != "Host" {
							for _, v := range vv {
								req.Header.Add(k, v)
							}
						}
					}
				} else {
					req.Header.Set("User-Agent", "VLC/3.0.18 LibVLC/3.0.18")
					req.Header.Set("Accept", "*/*")
				}

				state.Lock.Lock()
				state.UpstreamStatus = "Connecting"
				state.Lock.Unlock()

				resp, rerr := UpstreamHTTPClient.Do(req)
				if rerr == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
					state.Lock.Lock()
					state.UpstreamStatus = "Downloading"
					for b := firstEndBlock; b < state.TotalBlocks; b++ {
						if state.BlockStates[b] != BlockCompleted {
							state.BlockStates[b] = BlockDownloading
						}
					}
					state.Lock.Unlock()

					buffer := make([]byte, 512*1024)
					bytesRead := int64(0)
					prefetchFailed := false

					for bytesRead < prefetchLength {
						select {
						case <-ctx.Done():
							prefetchFailed = true
							break
						default:
						}
						if prefetchFailed {
							break
						}

						limit := int64(len(buffer))
						if prefetchLength-bytesRead < limit {
							limit = prefetchLength - bytesRead
						}

						n, readErr := resp.Body.Read(buffer[:limit])
						if n > 0 {
							writeOffset := prefetchStart + bytesRead
							_, werr := file.WriteAt(buffer[:n], writeOffset)
							if werr != nil {
								log.Printf("[Worker-%d] Prefetch file write error: %v", w.id, werr)
								prefetchFailed = true
								break
							}

							if !GlobalConfig.DisableFadvise {
								FadviseDontNeed(file.Fd(), writeOffset, int64(n))
							}

							bytesRead += int64(n)

							state.Lock.Lock()
							for b := firstEndBlock; b < state.TotalBlocks; b++ {
								bStart := int64(b) * blockSize
								bEnd := bStart + blockSize
								if bEnd > state.TotalSize {
									bEnd = state.TotalSize
								}
								if prefetchStart+bytesRead >= bEnd && state.BlockStates[b] == BlockDownloading {
									state.BlockStates[b] = BlockCompleted
									for _, ch := range state.Clients {
										safeSignal(ch)
									}
								}
							}
							state.Lock.Unlock()
						}

						if readErr != nil {
							if bytesRead < prefetchLength {
								log.Printf("[Worker-%d] Prefetch read error: %v", w.id, readErr)
								prefetchFailed = true
							}
							break
						}
					}
					_ = resp.Body.Close()

					state.Lock.Lock()
					for b := firstEndBlock; b < state.TotalBlocks; b++ {
						bStart := int64(b) * blockSize
						bEnd := bStart + blockSize
						if bEnd > state.TotalSize {
							bEnd = state.TotalSize
						}
						if prefetchStart+bytesRead >= bEnd {
							state.BlockStates[b] = BlockCompleted
						} else {
							if state.BlockStates[b] != BlockCompleted {
								state.BlockStates[b] = BlockNotStarted
							}
						}
					}
					for _, ch := range state.Clients {
						safeSignal(ch)
					}
					state.Lock.Unlock()
				} else {
					if rerr == nil {
						_ = resp.Body.Close()
					}
					log.Printf("[Worker-%d] Prefetch request failed. Status: %v, err: %v", w.id, resp, rerr)
				}
			} else {
				log.Printf("[Worker-%d] Prefetch request creation failed: %v", w.id, err)
			}

			atomic.StoreInt32(&w.isPrefetching, 0)
			log.Printf("[Worker-%d] Pre-fetching finished.", w.id)

			select {
			case state.NotifyChan <- struct{}{}:
			default:
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			state.Lock.Lock()
			if state.BlockStates[currentBlock] != BlockCompleted {
				state.BlockStates[currentBlock] = BlockNotStarted
			}
			state.Lock.Unlock()
			return
		default:
		}

		blockSize := GlobalConfig.BlockSize
		blockStartOffset := int64(currentBlock) * blockSize
		blockEndOffset := blockStartOffset + blockSize
		if blockEndOffset > state.TotalSize {
			blockEndOffset = state.TotalSize
		}
		blockLength := blockEndOffset - blockStartOffset

		if activeRespBody == nil {
			req, err := http.NewRequestWithContext(ctx, "GET", state.URL, nil)
			if err != nil {
				if handleFailure(false, "[Worker-%d] Error creating request: %v", w.id, err) {
					return
				}
				continue
			}

			rangeHeader := fmt.Sprintf("bytes=%d-%d", blockStartOffset, state.TotalSize-1)
			req.Header.Set("Range", rangeHeader)

			if initialHeader != nil && len(initialHeader) > 0 {
				for k, vv := range initialHeader {
					if k != "Range" && k != "Connection" && k != "Host" {
						for _, v := range vv {
							req.Header.Add(k, v)
						}
					}
				}
			} else {
				req.Header.Set("User-Agent", "VLC/3.0.18 LibVLC/3.0.18")
				req.Header.Set("Accept", "*/*")
			}

			state.Lock.Lock()
			state.UpstreamStatus = "Connecting"
			state.Lock.Unlock()

			log.Printf("[Worker-%d] Connecting upstream for block %d (Range: %s)", w.id, currentBlock, rangeHeader)
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

			activeRespBody = resp.Body
			
			state.Lock.Lock()
			state.UpstreamStatus = "Downloading"
			state.Lock.Unlock()
			
			consecutiveFailures = 0
			backoff = 2 * time.Second

			atomic.StoreInt64(&w.connStartTimeNano, time.Now().UnixNano())
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

		state.Lock.Lock()
		state.BlockStates[currentBlock] = BlockDownloading
		state.Lock.Unlock()

		blockBytesWritten := int64(0)
		buffer := make([]byte, 512*1024)
		blockFailed := false
		
		atomic.StoreInt64(&w.offset, blockStartOffset)
		startTime := time.Now()

		for blockBytesWritten < blockLength {
			select {
			case <-ctx.Done():
				state.Lock.Lock()
				if state.BlockStates[currentBlock] != BlockCompleted {
					state.BlockStates[currentBlock] = BlockNotStarted
				}
				state.Lock.Unlock()
				return
			default:
			}

			limit := int64(len(buffer))
			if blockLength-blockBytesWritten < limit {
				limit = blockLength - blockBytesWritten
			}

			n, err := activeRespBody.Read(buffer[:limit])
			if n > 0 {
				writeOffset := blockStartOffset + blockBytesWritten
				_, werr := file.WriteAt(buffer[:n], writeOffset)
				if werr != nil {
					log.Printf("[Worker-%d] File write error at %d: %v", w.id, writeOffset, werr)
					blockFailed = true
					break
				}

				if !GlobalConfig.DisableFadvise {
					FadviseDontNeed(file.Fd(), writeOffset, int64(n))
				}

				blockBytesWritten += int64(n)
				atomic.StoreInt64(&w.offset, blockStartOffset+blockBytesWritten)
				atomic.AddInt64(&w.connBytesWritten, int64(n))

				atomic.AddInt64(&state.BytesDownloadedSec, int64(n))
				GlobalRegistry.UpdateSize(int64(n))
			}

			if err != nil {
				if blockBytesWritten < blockLength {
					log.Printf("[Worker-%d] Upstream read error: %v", w.id, err)
					blockFailed = true
				}
				closeConnection()
				break
			}
		}

		elapsed := time.Since(startTime).Seconds()
		if elapsed > 0 {
			atomic.StoreInt64(&w.speed, int64(float64(blockBytesWritten)/elapsed))
		}

		if blockFailed {
			state.Lock.Lock()
			if state.BlockStates[currentBlock] != BlockCompleted {
				state.BlockStates[currentBlock] = BlockNotStarted
			}
			state.Lock.Unlock()
			closeConnection()
			
			if handleFailure(false, "[Worker-%d] Download block %d failed", w.id, currentBlock) {
				return
			}
			continue
		}

		if blockBytesWritten == blockLength {
			state.Lock.Lock()
			state.BlockStates[currentBlock] = BlockCompleted
			log.Printf("[Worker-%d] Block %d completed", w.id, currentBlock)

			for _, ch := range state.Clients {
				safeSignal(ch)
			}

			// Notify coordinator loop to wake up and check if it should preempt / finalize / check keep-alive
			select {
			case state.NotifyChan <- struct{}{}:
			default:
			}

			// We REMOVED the intermediate metadata JSON file write from here to prevent redundant disk I/O!

			nextBlock := currentBlock + 1
			if nextBlock >= len(state.BlockStates) {
				state.Lock.Unlock()
				closeConnection()
				return
			}

			nextStatus := state.BlockStates[nextBlock]
			if nextStatus == BlockCompleted {
				log.Printf("[Worker-%d] Next block %d already completed. Worker coalescing and exiting.", w.id, nextBlock)
				state.Lock.Unlock()
				closeConnection()
				return
			}
			
			if nextStatus == BlockDownloading {
				isAnotherWorkerDownloading := false
				state.WorkersLock.Lock()
				for _, aw := range state.ActiveWorkers {
					if aw.id != w.id && aw.currentBlock == nextBlock {
						isAnotherWorkerDownloading = true
						break
					}
				}
				state.WorkersLock.Unlock()

				if isAnotherWorkerDownloading {
					log.Printf("[Worker-%d] Next block %d already downloading by another worker. Exiting.", w.id, nextBlock)
					state.Lock.Unlock()
					closeConnection()
					return
				}
			}

			currentBlock = nextBlock
			w.currentBlock = currentBlock
			state.Lock.Unlock()
		}
	}
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
