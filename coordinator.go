package main

import (
	"context"
	"encoding/binary"
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
	for i, st := range state.BlockStates {
		if st == BlockDownloading {
			state.BlockStates[i] = BlockNotStarted
		}
	}
	clientsCount := len(state.Clients)
	fileToClose := state.File
	state.File = nil
	state.WriterActive = false
	partPath := state.PartPath
	metaPath := state.MetaPath
	key := state.Key
	state.Lock.Unlock()

	if GlobalLogBroker != nil {
		go GlobalLogBroker.BroadcastStateUpdate()
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
	startBlock        int
	cancel            context.CancelFunc
	currentBlock      int
	offset            int64 // guarded atomically
	speed             int64 // estimated speed (bytes/sec)
	connStartTimeNano int64 // Unix nano timestamp of connection start, guarded atomically
	connBytesWritten   int64 // bytes read on this connection, guarded atomically
	isPrefetching     int32 // 1 if prefetching last blocks (metadata), 0 otherwise, guarded atomically
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
	sniffed := false

	defer func() {
		parentCancel()
		state.Lock.Lock()
		state.Cancel = nil

		// Wake up any client readers waiting on block updates
		for _, ch := range state.Clients {
			safeSignal(ch)
		}

		// We check if all blocks are completed to ensure we only finalize fully completed files.
		isCompleted := len(state.BlockStates) > 0
		for _, status := range state.BlockStates {
			if status != BlockCompleted {
				isCompleted = false
				break
			}
		}

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
			// Incomplete download (state.Lock is already held from line 138)
			for i, st := range state.BlockStates {
				if st == BlockDownloading {
					state.BlockStates[i] = BlockNotStarted
				}
			}
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

	spawnPrefetchWorker := func(startBlock int) {
		state.WorkersLock.Lock()
		defer state.WorkersLock.Unlock()
		workerIdGen++
		wCtx, wCancel := context.WithCancel(parentCtx)
		w := &Worker{
			id:               workerIdGen,
			startBlock:       startBlock,
			cancel:           wCancel,
			currentBlock:     startBlock,
			isPrefetchHelper: true,
		}
		atomic.StoreInt32(&w.isPrefetching, 1)
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
		// Sniff check for container optimization
		state.Lock.Lock()
		block0Completed := len(state.BlockStates) > 0 && state.BlockStates[0] == BlockCompleted
		state.Lock.Unlock()

		if block0Completed && !sniffed {
			sniffed = true
			log.Printf("[Coord] Block 0 completed. Sniffing media headers to determine optimization status...")
			go func() {
				buf := make([]byte, 128*1024)
				n, err := file.ReadAt(buf, 0)
				if (err == nil || err == io.EOF) && n > 0 {
					data := buf[:n]
					isFastStart := false
					moovOffset := int64(0)
					hasMdatSize := false
					hasCues := false
					cuesOffset := int64(0)
					prefetchBlocks := 0

					isMkv := n >= 4 && binary.BigEndian.Uint32(data[0:4]) == 0x1A45DFA3
					isMp4 := false

					if isMkv {
						hasCues, cuesOffset = GetMkvCuesOffset(data)
						state.Lock.Lock()
						state.ContentType = "video/x-matroska"
						state.Lock.Unlock()
					} else if n >= 8 {
						firstBoxType := string(data[4:8])
						if firstBoxType == "ftyp" || firstBoxType == "free" || firstBoxType == "moov" || firstBoxType == "mdat" {
							isMp4 = true
							isFastStart, moovOffset, hasMdatSize = IsMp4FastStart(data)
							state.Lock.Lock()
							state.ContentType = "video/mp4"
							state.Lock.Unlock()
						}
					}

					padding := GlobalConfig.GetSchedulerMinBlocks()
					if padding < 1 {
						padding = 1
					}

					var tailStartBlock int = -1
					if isMp4 && !isFastStart {
						blockSize := GlobalConfig.GetBlockSize()
						maxTailBlocks := GlobalConfig.GetMaxPrefetchTailBlocks()
						if hasMdatSize && moovOffset > 0 {
							moovBytes := state.TotalSize - moovOffset
							prefetchBlocks = int((moovBytes + blockSize - 1) / blockSize) + padding
						} else {
							// Estimate based on 0.15% of file size, floor of 16MB
							estimatedBytes := state.TotalSize * 15 / 10000
							if estimatedBytes < 16*1024*1024 {
								estimatedBytes = 16*1024*1024
							}
							prefetchBlocks = int((estimatedBytes + blockSize - 1) / blockSize) + padding
						}

						if prefetchBlocks > maxTailBlocks {
							prefetchBlocks = maxTailBlocks
						}
						tailStartBlock = state.TotalBlocks - prefetchBlocks
					} else if isMkv && hasCues && cuesOffset > 0 {
						blockSize := GlobalConfig.GetBlockSize()
						maxTailBlocks := GlobalConfig.GetMaxPrefetchTailBlocks()
						cuesBytes := state.TotalSize - cuesOffset
						prefetchBlocks = int((cuesBytes + blockSize - 1) / blockSize) + padding

						if prefetchBlocks > maxTailBlocks {
							prefetchBlocks = maxTailBlocks
						}
						tailStartBlock = state.TotalBlocks - prefetchBlocks
					}

					if tailStartBlock < 0 {
						tailStartBlock = 0
					}

					// Only pre-fetch if the start block is far enough from the beginning (lockout blocks).
					// If the calculated tailStartBlock falls inside the initial block download run
					// guaranteed by SchedulerMinBlocks, the sequential worker will pull it anyway.
					lockoutBlocks := GlobalConfig.GetSchedulerMinBlocks() * 2
					if lockoutBlocks < 2 {
						lockoutBlocks = 2
					}

					if tailStartBlock >= lockoutBlocks && prefetchBlocks < state.TotalBlocks/2 && tailStartBlock < state.TotalBlocks {
						needPrefetch := false
						state.Lock.Lock()
						for b := tailStartBlock; b < state.TotalBlocks; b++ {
							if state.BlockStates[b] != BlockCompleted {
								needPrefetch = true
								break
							}
						}
						state.Lock.Unlock()

						if needPrefetch {
							maxWorkers := GlobalConfig.GetMaxUpstreamWorkers()
							if maxWorkers > 1 {
								log.Printf("[Coord] Sniffed non-optimized container. Spawning parallel helper worker to pre-fetch metadata starting at block %d", tailStartBlock)
								spawnPrefetchWorker(tailStartBlock)
							} else {
								log.Printf("[Coord] Sniffed non-optimized container. MaxUpstreamWorkers=1: Queuing inline tail pre-fetch starting at block %d", tailStartBlock)
								state.Lock.Lock()
								state.PendingTailPrefetchBlock = tailStartBlock
								state.Lock.Unlock()
								select {
								case state.NotifyChan <- struct{}{}:
								default:
								}
							}
						} else {
							log.Printf("[Coord] Metadata tail blocks already cached. Skipping pre-fetch.")
						}
					} else {
						log.Printf("[Coord] Sniffed optimized/faststart or unsupported container. Skipping pre-fetch.")
					}
				} else {
					log.Printf("[Coord] Error reading block 0 for sniffing: %v (read %d bytes)", err, n)
				}
			}()
		}

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

						// If the worker is currently pre-fetching tail blocks inline and sb is within the tail range:
						if atomic.LoadInt32(&w.isPrefetching) == 1 && sb >= wCurrent {
							log.Printf("[Coord] Worker %d is pre-fetching tail blocks (at %d, client starving at %d). Suppressing pre-emption.", 
								w.id, wCurrent, sb)
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
									blockSize := GlobalConfig.GetBlockSize()
									hydrationWait := GlobalConfig.GetClientHydrationWait()
									targetOffset := int64(sb) * blockSize
									if wOffset <= targetOffset {
										bytesRemaining := (int64(sb)+1)*blockSize - wOffset
										eta := time.Duration(float64(bytesRemaining)/float64(wSpeed)) * time.Second
										if eta < hydrationWait {
											log.Printf("[Coord] Client Hydration Wait active for block %d (ETA %.2fs < threshold %s). Suppressing connection jump.", 
												sb, eta.Seconds(), hydrationWait)
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

					maxWorkers := GlobalConfig.GetMaxUpstreamWorkers()
					if activeCount < maxWorkers {
						log.Printf("[Coord] Spawning new worker starting at starving block %d (Active: %d/%d)", 
							sb, activeCount+1, maxWorkers)
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
							
							minTime := GlobalConfig.GetSchedulerMinTime()
							minBlocks := GlobalConfig.GetSchedulerMinBlocks()
							blockSize := GlobalConfig.GetBlockSize()
							eligible := false
							if minTime <= 0 && minBlocks <= 0 {
								eligible = true
							} else {
								timeOk := minTime > 0 && elapsed >= minTime
								bytesOk := minBlocks > 0 && connBytes >= int64(minBlocks)*blockSize
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

	if w.isPrefetchHelper {
		blockSize := GlobalConfig.GetBlockSize()
		prefetchStart := int64(w.startBlock) * blockSize
		prefetchLength := state.TotalSize - prefetchStart

		log.Printf("[Worker-%d] Parallel Pre-fetch Helper: Grabbing last %d bytes starting at block %d", w.id, prefetchLength, w.startBlock)

		req, err := http.NewRequestWithContext(ctx, "GET", state.URL, nil)
		if err == nil {
			rangeHeader := fmt.Sprintf("bytes=%d-%d", prefetchStart, state.TotalSize-1)
			req.Header.Set("Range", rangeHeader)
			CopySafeHeaders(initialHeader, req.Header)

			state.SetUpstreamStatus("Connecting")

			resp, rerr := UpstreamHTTPClient.Do(req)
			if rerr == nil && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent) {
				state.SetUpstreamStatus("Downloading")
				state.Lock.Lock()
				var downloadingBlocks []int
				for b := w.startBlock; b < state.TotalBlocks; b++ {
					if state.BlockStates[b] != BlockCompleted {
						state.BlockStates[b] = BlockDownloading
						downloadingBlocks = append(downloadingBlocks, b)
					}
				}
				if len(downloadingBlocks) > 0 && GlobalLogBroker != nil {
					GlobalLogBroker.BroadcastBlockUpdate(state.Key, downloadingBlocks, BlockDownloading)
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

						if !GlobalConfig.GetDisableFadvise() {
							FadviseDontNeed(file.Fd(), writeOffset, int64(n))
						}

						bytesRead += int64(n)

						atomic.AddInt64(&state.BytesDownloadedSec, int64(n))
						atomic.AddInt64(&state.TotalBytesDownloaded, int64(n))
						GlobalRegistry.UpdateSize(int64(n))

						state.Lock.Lock()
						for b := w.startBlock; b < state.TotalBlocks; b++ {
							bStart := int64(b) * blockSize
							bEnd := bStart + blockSize
							if bEnd > state.TotalSize {
								bEnd = state.TotalSize
							}
							if prefetchStart+bytesRead >= bEnd && state.BlockStates[b] == BlockDownloading {
								state.BlockStates[b] = BlockCompleted
								if GlobalLogBroker != nil {
									GlobalLogBroker.BroadcastBlockUpdate(state.Key, []int{b}, BlockCompleted)
								}
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
				for b := w.startBlock; b < state.TotalBlocks; b++ {
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

				state.Lock.Lock()
				for b := w.startBlock; b < state.TotalBlocks; b++ {
					if state.BlockStates[b] != BlockCompleted {
						state.BlockStates[b] = BlockNotStarted
					}
				}
				state.Lock.Unlock()
			}
		} else {
			log.Printf("[Worker-%d] Prefetch request creation failed: %v", w.id, err)
		}

		atomic.StoreInt32(&w.isPrefetching, 0)
		log.Printf("[Worker-%d] Parallel Pre-fetching finished.", w.id)

		select {
		case state.NotifyChan <- struct{}{}:
		default:
		}
		return
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

		// Check for pending inline tail pre-fetch when MaxUpstreamWorkers == 1
		state.Lock.Lock()
		pendingTail := state.PendingTailPrefetchBlock
		if pendingTail > 0 {
			state.PendingTailPrefetchBlock = -1
			if state.BlockStates[currentBlock] != BlockCompleted {
				state.BlockStates[currentBlock] = BlockNotStarted
				if GlobalLogBroker != nil {
					GlobalLogBroker.BroadcastBlockUpdate(state.Key, []int{currentBlock}, BlockNotStarted)
				}
			}
		}
		state.Lock.Unlock()

		if pendingTail > 0 {
			closeConnection()
			runInlineTailPrefetch(ctx, w, state, initialHeader, file, pendingTail)
			continue
		}

		blockSize := GlobalConfig.GetBlockSize()
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

			CopySafeHeaders(initialHeader, req.Header)
			if ua := req.Header.Get("User-Agent"); ua != "" {
				state.Lock.Lock()
				state.UpstreamUserAgent = ua
				state.Lock.Unlock()
			}

			state.SetUpstreamStatus("Connecting")

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
			
			state.SetUpstreamStatus("Downloading")
			
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
		if GlobalLogBroker != nil {
			GlobalLogBroker.BroadcastBlockUpdate(state.Key, []int{currentBlock}, BlockDownloading)
		}

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

				if !GlobalConfig.GetDisableFadvise() {
					FadviseDontNeed(file.Fd(), writeOffset, int64(n))
				}

				blockBytesWritten += int64(n)
				atomic.StoreInt64(&w.offset, blockStartOffset+blockBytesWritten)
				atomic.AddInt64(&w.connBytesWritten, int64(n))

				atomic.AddInt64(&state.BytesDownloadedSec, int64(n))
				atomic.AddInt64(&state.TotalBytesDownloaded, int64(n))
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
			if GlobalLogBroker != nil {
				GlobalLogBroker.BroadcastBlockUpdate(state.Key, []int{currentBlock}, BlockCompleted)
			}

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

func runInlineTailPrefetch(
	ctx context.Context,
	w *Worker,
	state *FileCacheState,
	initialHeader http.Header,
	file *os.File,
	tailStartBlock int,
) {
	blockSize := GlobalConfig.GetBlockSize()
	prefetchStart := int64(tailStartBlock) * blockSize
	prefetchLength := state.TotalSize - prefetchStart
	if prefetchLength <= 0 {
		return
	}

	atomic.StoreInt32(&w.isPrefetching, 1)
	w.currentBlock = tailStartBlock
	defer atomic.StoreInt32(&w.isPrefetching, 0)

	log.Printf("[Worker-%d] MaxUpstreamWorkers=1: Executing inline tail pre-fetch (%d bytes) starting at block %d", w.id, prefetchLength, tailStartBlock)

	req, err := http.NewRequestWithContext(ctx, "GET", state.URL, nil)
	if err != nil {
		log.Printf("[Worker-%d] Inline tail pre-fetch request creation failed: %v", w.id, err)
		return
	}

	rangeHeader := fmt.Sprintf("bytes=%d-%d", prefetchStart, state.TotalSize-1)
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
	state.Lock.Lock()
	var downloadingBlocks []int
	for b := tailStartBlock; b < state.TotalBlocks; b++ {
		if state.BlockStates[b] != BlockCompleted {
			state.BlockStates[b] = BlockDownloading
			downloadingBlocks = append(downloadingBlocks, b)
		}
	}
	if len(downloadingBlocks) > 0 && GlobalLogBroker != nil {
		GlobalLogBroker.BroadcastBlockUpdate(state.Key, downloadingBlocks, BlockDownloading)
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
				log.Printf("[Worker-%d] Inline tail pre-fetch file write error: %v", w.id, werr)
				prefetchFailed = true
				break
			}

			if !GlobalConfig.GetDisableFadvise() {
				FadviseDontNeed(file.Fd(), writeOffset, int64(n))
			}

			bytesRead += int64(n)

			atomic.AddInt64(&state.BytesDownloadedSec, int64(n))
			atomic.AddInt64(&state.TotalBytesDownloaded, int64(n))
			GlobalRegistry.UpdateSize(int64(n))

			state.Lock.Lock()
			for b := tailStartBlock; b < state.TotalBlocks; b++ {
				bStart := int64(b) * blockSize
				bEnd := bStart + blockSize
				if bEnd > state.TotalSize {
					bEnd = state.TotalSize
				}
				if prefetchStart+bytesRead >= bEnd && state.BlockStates[b] == BlockDownloading {
					state.BlockStates[b] = BlockCompleted
					if GlobalLogBroker != nil {
						GlobalLogBroker.BroadcastBlockUpdate(state.Key, []int{b}, BlockCompleted)
					}
					for _, ch := range state.Clients {
						safeSignal(ch)
					}
				}
			}
			state.Lock.Unlock()
		}

		if readErr != nil {
			if bytesRead < prefetchLength {
				log.Printf("[Worker-%d] Inline tail pre-fetch read error: %v", w.id, readErr)
			}
			break
		}
	}

	state.Lock.Lock()
	var resetBlocks []int
	for b := tailStartBlock; b < state.TotalBlocks; b++ {
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
				resetBlocks = append(resetBlocks, b)
			}
		}
	}
	if len(resetBlocks) > 0 && GlobalLogBroker != nil {
		GlobalLogBroker.BroadcastBlockUpdate(state.Key, resetBlocks, BlockNotStarted)
	}
	for _, ch := range state.Clients {
		safeSignal(ch)
	}
	state.Lock.Unlock()

	log.Printf("[Worker-%d] Inline tail pre-fetch finished. Resuming sequential stream.", w.id)
}
