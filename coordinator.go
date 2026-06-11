package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
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

func runCoordinator(state *FileCacheState, initialHeader http.Header) {
	log.Printf("[Coord] Starting background coordinator for cache: %s", state.Key)

	defer func() {
		state.Lock.Lock()
		state.WriterActive = false

		// We check if all blocks are completed to ensure we only finalize fully completed files.
		isCompleted := len(state.BlockStates) > 0
		for _, status := range state.BlockStates {
			if status != BlockCompleted {
				isCompleted = false
				break
			}
		}

		shouldFinalize := len(state.Clients) == 0 && isCompleted && state.File != nil && !FileExists(state.Path)
		if shouldFinalize {
			_ = state.File.Close()
			state.File = nil
		}
		state.Lock.Unlock()

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
				log.Printf("[Coord] Cached file finalized on coordinator exit: %s", state.Key)
			} else {
				log.Printf("[Coord] Error renaming cache file on coordinator exit: %v", err)
			}
		}
		log.Printf("[Coord] Exiting background coordinator for cache: %s", state.Key)
	}()

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
		return
	}
	_ = file.Truncate(state.TotalSize)

	state.Lock.Lock()
	state.File = file
	state.Lock.Unlock()

	var activeRespBody io.ReadCloser
	var currentUpstreamURL string
	var currentBlockIndex = -1
	var connStartTime time.Time
	var connBytesWritten int64

	// Helper to close upstream stream
	closeUpstream := func() {
		if activeRespBody != nil {
			_ = activeRespBody.Close()
			activeRespBody = nil
			log.Printf("[Coord] Closed active upstream connection for %s", state.Key)
		}
	}
	defer closeUpstream()

	// Main Coordinator Loop
	for {
		// 1. Choose target block to download
		state.Lock.Lock()

		// Check if anyone is starving (needs blocks)
		targetBlock := -1
		for _, starvingBlock := range state.Starving {
			if state.BlockStates[starvingBlock] != BlockCompleted {
				targetBlock = starvingBlock
				break // pick first starving client
			}
		}

		// If no one is starving, find the next uncompleted block (sequential gap-filler)
		if targetBlock == -1 {
			for i, status := range state.BlockStates {
				if status != BlockCompleted {
					targetBlock = i
					break
				}
			}
		}

		// If all blocks are completed, we are done!
		if targetBlock == -1 {
			state.Lock.Unlock()
			log.Printf("[Coord] All blocks completed for cache: %s", state.Key)
			break
		}

		// Evaluate jump condition (debounce check)
		shouldJump := false
		if currentBlockIndex != -1 && targetBlock != currentBlockIndex {
			// If it's the next adjacent sequential block, we never jump/reconnect.
			// We let the current connection flow naturally.
			if targetBlock == currentBlockIndex+1 {
				shouldJump = false
			} else {
				// If no client is starving for the current block, we bypass the debouncer
				currentBlockBusy := false
				for _, sb := range state.Starving {
					if sb == currentBlockIndex {
						currentBlockBusy = true
						break
					}
				}

				if !currentBlockBusy {
					shouldJump = true
					log.Printf("[Coord] No clients starving for current block %d. Jumping immediately to block %d", currentBlockIndex, targetBlock)
				} else {
					elapsed := time.Since(connStartTime)
					var debouncerOk bool
					if GlobalConfig.SchedulerMinTime <= 0 && GlobalConfig.SchedulerMinBytes <= 0 {
						debouncerOk = true
					} else {
						timeOk := GlobalConfig.SchedulerMinTime > 0 && elapsed >= GlobalConfig.SchedulerMinTime
						bytesOk := GlobalConfig.SchedulerMinBytes > 0 && connBytesWritten >= GlobalConfig.SchedulerMinBytes
						debouncerOk = timeOk || bytesOk
					}

					if debouncerOk {
						shouldJump = true
						log.Printf("[Coord] Debouncer allows jump: from block %d to %d (elapsed=%s, bytes=%dKB)",
							currentBlockIndex, targetBlock, elapsed.Truncate(time.Millisecond), connBytesWritten/1024)
					}
				}
			}
		}

		// If we need to jump or start a new connection
		if activeRespBody == nil || shouldJump {
			closeUpstream()

			currentBlockIndex = targetBlock
			connStartTime = time.Now()
			connBytesWritten = 0

			// Start download at target block offset
			blockSize := GlobalConfig.BlockSize
			startOffset := int64(currentBlockIndex) * blockSize

			// Build upstream request using context.Background
			req, err := http.NewRequestWithContext(context.Background(), "GET", state.URL, nil)
			if err != nil {
				state.Lock.Unlock()
				log.Printf("[Coord] Error creating request: %v", err)
				time.Sleep(1 * time.Second) // rate-limit retries
				continue
			}

			// Add range header
			rangeHeader := fmt.Sprintf("bytes=%d-%d", startOffset, state.TotalSize-1)
			req.Header.Set("Range", rangeHeader)

			// Spoof headers: Clone from client request if available, or fall back to VLC
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

			state.Lock.Unlock()

			log.Printf("[Coord] Connecting upstream for block %d (Range: %s)", currentBlockIndex, rangeHeader)
			resp, err := UpstreamHTTPClient.Do(req)
			if err != nil {
				log.Printf("[Coord] Upstream request failed: %v", err)
				time.Sleep(1 * time.Second)
				continue
			}

			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				log.Printf("[Coord] Upstream returned error status: %d", resp.StatusCode)
				_ = resp.Body.Close()
				time.Sleep(1 * time.Second)
				continue
			}

			activeRespBody = resp.Body
			currentUpstreamURL = resp.Request.URL.String()
			log.Printf("[Coord] Connected successfully to CDN: %s", currentUpstreamURL)
			continue
		}

		state.Lock.Unlock()

		// 2. Download current block
		blockSize := GlobalConfig.BlockSize
		blockStartOffset := int64(currentBlockIndex) * blockSize
		blockEndOffset := blockStartOffset + blockSize
		if blockEndOffset > state.TotalSize {
			blockEndOffset = state.TotalSize
		}

		// Read and write chunks for this block
		blockBytesWritten := int64(0)
		blockLength := blockEndOffset - blockStartOffset
		buffer := make([]byte, 64*1024) // 64KB chunk buffer

		state.Lock.Lock()
		state.BlockStates[currentBlockIndex] = BlockDownloading
		state.Lock.Unlock()

		blockFailed := false

		for blockBytesWritten < blockLength {
			// Check if a client is starving for a different block and we can jump
			state.Lock.Lock()
			hasStarvation := false
			starvingBlock := -1
			for _, sb := range state.Starving {
				if sb != currentBlockIndex && state.BlockStates[sb] != BlockCompleted {
					hasStarvation = true
					starvingBlock = sb
					break
				}
			}

			if hasStarvation && starvingBlock != currentBlockIndex+1 {
				currentBlockBusy := false
				for _, sb := range state.Starving {
					if sb == currentBlockIndex {
						currentBlockBusy = true
						break
					}
				}

				var debouncerOk bool
				if !currentBlockBusy {
					debouncerOk = true
					log.Printf("[Coord] No clients starving for current block %d. Pre-empting to jump to block %d", currentBlockIndex, starvingBlock)
				} else {
					elapsed := time.Since(connStartTime)
					if GlobalConfig.SchedulerMinTime <= 0 && GlobalConfig.SchedulerMinBytes <= 0 {
						debouncerOk = true
					} else {
						timeOk := GlobalConfig.SchedulerMinTime > 0 && elapsed >= GlobalConfig.SchedulerMinTime
						bytesOk := GlobalConfig.SchedulerMinBytes > 0 && connBytesWritten >= GlobalConfig.SchedulerMinBytes
						debouncerOk = timeOk || bytesOk
					}
				}

				if debouncerOk {
					// Debouncer allows jump! Break out of block loop
					state.Lock.Unlock()
					log.Printf("[Coord] Pre-empting current block %d download to jump to starving block %d", currentBlockIndex, starvingBlock)
					break
				}
			}
			state.Lock.Unlock()

			// Read chunk from upstream
			limit := int64(len(buffer))
			if blockLength-blockBytesWritten < limit {
				limit = blockLength - blockBytesWritten
			}

			n, err := activeRespBody.Read(buffer[:limit])
			if n > 0 {
				writeOffset := blockStartOffset + blockBytesWritten
				_, werr := file.WriteAt(buffer[:n], writeOffset)
				if werr != nil {
					log.Printf("[Coord] File write error at %d: %v", writeOffset, werr)
					blockFailed = true
					break
				}

				// Purge page cache to keep Docker container memory flat
				FadviseDontNeed(file.Fd(), writeOffset, int64(n))

				blockBytesWritten += int64(n)
				connBytesWritten += int64(n)

				// Update running cache size O(1)
				GlobalRegistry.UpdateSize(int64(n))
			}

			if err != nil {
				if err != io.EOF && blockBytesWritten < blockLength {
					log.Printf("[Coord] Upstream read error: %v", err)
					blockFailed = true
				}
				closeUpstream()
				break
			}
		}

		if blockFailed {
			state.Lock.Lock()
			if state.BlockStates[currentBlockIndex] != BlockCompleted {
				state.BlockStates[currentBlockIndex] = BlockNotStarted
			}
			state.Lock.Unlock()

			closeUpstream()
			time.Sleep(1 * time.Second)
			continue
		}

		// If the block is completed fully, mark it and notify clients
		if blockBytesWritten == blockLength {
			state.Lock.Lock()
			state.BlockStates[currentBlockIndex] = BlockCompleted
			log.Printf("[Coord] Block %d completed (Key: %s)", currentBlockIndex, state.Key)

			// Notify waiting clients
			for _, ch := range state.Clients {
				select {
				case ch <- struct{}{}:
				default:
				}
			}

			// Save block completion status in metadata JSON file
			meta := FileMeta{
				URL:           state.URL,
				TotalSize:     state.TotalSize,
				ETag:          state.ETag,
				LastModified:  state.LastModified,
				LastValidated: time.Now().Unix(),
				Completed:     false,
				BlockStates:   state.BlockStates,
			}
			metaBytes, err := json.Marshal(meta)
			if err == nil {
				_ = os.WriteFile(state.MetaPath, metaBytes, 0644)
			}

			state.Lock.Unlock()
			currentBlockIndex++ // Move sequentially forward
		} else {
			// We exited early (likely due to a jump). Reset block status to NotStarted
			// if it wasn't fully completed.
			state.Lock.Lock()
			if state.BlockStates[currentBlockIndex] != BlockCompleted {
				state.BlockStates[currentBlockIndex] = BlockNotStarted
			}
			state.Lock.Unlock()
		}

		// 3. Check for jump requests / starve notifications
		// If we are waiting and idle, block on NotifyChan.
		state.Lock.Lock()
		activeStarvingCount := 0
		for _, sb := range state.Starving {
			if state.BlockStates[sb] != BlockCompleted {
				activeStarvingCount++
			}
		}
		state.Lock.Unlock()

		if activeStarvingCount == 0 && currentBlockIndex >= len(state.BlockStates) {
			// We have finished downloading our current range and no clients are starving.
			// Let's loop back to check if there are sequential gaps left.
			closeUpstream()
			continue
		}

		// Sleep or check NotifyChan to avoid tight spinning
		select {
		case <-state.NotifyChan:
			// Client starvation woke us up. Loop back immediately to evaluate.
		default:
			// Run next loop iteration
		}
	}
}
