package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"time"
)

type CacheReader struct {
	state    *FileCacheState
	offset   int64
	clientID string
	ctx      context.Context
}

func NewCacheReader(ctx context.Context, state *FileCacheState, req *http.Request) *CacheReader {
	state.Lock.Lock()
	defer state.Lock.Unlock()

	// Generate a unique client ID
	clientID := fmt.Sprintf("client-%d", rand.Intn(1000000))
	state.Clients[clientID] = make(chan struct{}, 1)

	var remoteAddr, userAgent string
	if req != nil {
		remoteAddr = req.RemoteAddr
		userAgent = req.Header.Get("User-Agent")
	}
	if userAgent == "" {
		userAgent = "Unknown"
	}
	if remoteAddr == "" {
		remoteAddr = "Internal"
	}

	// Check if all blocks are already completed to set InCache correctly
	isCompleted := len(state.BlockStates) > 0
	for _, status := range state.BlockStates {
		if status != BlockCompleted {
			isCompleted = false
			break
		}
	}

	conn := &ClientConnection{
		ID:            clientID,
		Key:           state.Key,
		URL:           state.URL,
		RemoteAddr:    remoteAddr,
		UserAgent:     userAgent,
		Offset:        0,
		TotalSize:     state.TotalSize,
		StartTime:     time.Now().Unix(),
		InCache:       isCompleted,
		StarvingBlock: -1,
	}
	GlobalRegistry.RegisterConnection(clientID, conn)

	r := &CacheReader{
		state:    state,
		clientID: clientID,
		ctx:      ctx,
	}

	log.Printf("[Reader] Registered client %s (%s, UA: %s) for media %s", clientID, remoteAddr, userAgent, state.Key)
	return r
}

func (r *CacheReader) Close() error {
	r.state.Lock.Lock()

	// Clean up client channels
	if ch, ok := r.state.Clients[r.clientID]; ok {
		close(ch)
		delete(r.state.Clients, r.clientID)
	}
	delete(r.state.Starving, r.clientID)

	log.Printf("[Reader] Unregistered client %s from media %s", r.clientID, r.state.Key)

	// Check if the file is completed and we are the last client exiting.
	// If so, we finalize the cache file on disk (handles Windows file locks).
	// We check if all blocks are completed to ensure we only finalize fully completed files.
	isCompleted := len(r.state.BlockStates) > 0
	for _, status := range r.state.BlockStates {
		if status != BlockCompleted {
			isCompleted = false
			break
		}
	}

	shouldFinalize := len(r.state.Clients) == 0 && isCompleted && !r.state.WriterActive && !FileExists(r.state.Path)
	r.state.Lock.Unlock()

	GlobalRegistry.UnregisterConnection(r.clientID)

	if shouldFinalize {
		r.finalizeCache()
	}

	return nil
}

func (r *CacheReader) finalizeCache() {
	// Close write handle if open
	r.state.Lock.Lock()
	if r.state.File != nil {
		_ = r.state.File.Close()
		r.state.File = nil
	}
	r.state.Lock.Unlock()

	// Rename temporary file to final cache path
	err := os.Rename(r.state.PartPath, r.state.Path)
	if err != nil {
		log.Printf("[Reader] Error renaming cache file on final client exit: %v", err)
		return
	}

	// Save final metadata JSON confirming completion
	metaPath := r.state.Path + ".json"
	meta := FileMeta{
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

	blockSize := GlobalConfig.BlockSize
	startBlock := int(off / blockSize)
	endBlock := int((off + int64(len(p)) - 1) / blockSize)

	// Wait for all required blocks to be Completed
	for b := startBlock; b <= endBlock; b++ {
		err := r.waitForBlock(b)
		if err != nil {
			return 0, err
		}
	}

	// Read directly from the file
	r.state.Lock.Lock()
	file := r.state.File
	r.state.Lock.Unlock()

	if file == nil {
		// If coordinator closed it, read from final completed path
		file, err = os.Open(r.state.Path)
		if err != nil {
			return 0, err
		}
		defer file.Close()
	}

	n, err = file.ReadAt(p, off)
	if n > 0 {
		// Update client read offset
		GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(n))

		// Check if file is fully completed (all blocks are completed)
		r.state.Lock.Lock()
		isCompleted := len(r.state.BlockStates) > 0
		for _, status := range r.state.BlockStates {
			if status != BlockCompleted {
				isCompleted = false
				break
			}
		}
		r.state.Lock.Unlock()

		if isCompleted {
			GlobalRegistry.SetConnectionInCache(r.clientID, true)
		}

		// Purge page cache on Linux to prevent memory bloat
		FadviseDontNeed(file.Fd(), off, int64(n))
	}
	return n, err
}

func (r *CacheReader) waitForBlock(blockIndex int) error {
	r.state.Lock.Lock()
	clientChan := r.state.Clients[r.clientID]
	r.state.Lock.Unlock()

	if clientChan == nil {
		r.state.Lock.Lock()
		delete(r.state.Starving, r.clientID)
		r.state.Lock.Unlock()
		GlobalRegistry.UnregisterConnection(r.clientID)
		return errors.New("client unregistered")
	}

	for {
		r.state.Lock.Lock()
		status := r.state.BlockStates[blockIndex]

		if status == BlockCompleted {
			delete(r.state.Starving, r.clientID)
			r.state.Lock.Unlock()
			GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
			return nil
		}

		// If the block is not started, we trigger an on-demand download request
		if status == BlockNotStarted {
			r.state.BlockStates[blockIndex] = BlockDownloading
			r.state.Starving[r.clientID] = blockIndex
			r.state.Lock.Unlock()

			GlobalRegistry.UpdateConnectionStarvation(r.clientID, blockIndex)

			// Signal the Coordinator to wake up and evaluate the Jump request
			select {
			case r.state.NotifyChan <- struct{}{}:
			default:
			}
		} else {
			// Mark client as starving for this block
			r.state.Starving[r.clientID] = blockIndex
			r.state.Lock.Unlock()

			GlobalRegistry.UpdateConnectionStarvation(r.clientID, blockIndex)
		}

		// Block and wait for a completion update OR request cancellation
		select {
		case <-clientChan:
			// Woken up by coordinator write. Check block status again.
		case <-r.ctx.Done():
			// Client disconnected! Unregister and return cancellation error
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			r.state.Lock.Unlock()
			return r.ctx.Err()
		}
	}
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
	return next, nil
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
