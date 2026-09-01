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
	"sync"
	"sync/atomic"
	"time"
)

var globalClientIDSeq uint64

type CacheReader struct {
	state    *FileCacheState
	offset   int64
	clientID string
	ctx      context.Context
	file     *os.File
	fileMu   sync.Mutex
}

func NewCacheReader(ctx context.Context, state *FileCacheState, req *http.Request) *CacheReader {
	state.Lock.Lock()

	// Generate a unique client ID using atomic counter
	clientID := fmt.Sprintf("client-%d", atomic.AddUint64(&globalClientIDSeq, 1))
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
		Offset:         startOffset,
		StartOffset:    startOffset,
		TotalSize:      totalSize,
		StartTime:      time.Now().Unix(),
		InCache:        isCompleted,
		StarvingOffset: -1,
	}
	GlobalRegistry.RegisterConnection(clientID, conn)

	r := &CacheReader{
		state:    state,
		clientID: clientID,
		ctx:      ctx,
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

func (r *CacheReader) Close() error {
	r.fileMu.Lock()
	if r.file != nil {
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

	log.Printf("[Reader] Unregistered client %s from media %s", r.clientID, r.state.Key)

	isCompleted := r.state.RangeSet.IsComplete(r.state.TotalSize)
	shouldFinalize := len(r.state.Clients) == 0 && isCompleted && !r.state.WriterActive && !FileExists(r.state.Path)
	shouldCleanUpIncomplete := len(r.state.Clients) == 0 && !isCompleted && !r.state.WriterActive

	var fileToClose *os.File
	if len(r.state.Clients) == 0 && !r.state.WriterActive && r.state.File != nil && !shouldFinalize {
		fileToClose = r.state.File
		r.state.File = nil
	}
	r.state.Lock.Unlock()

	GlobalRegistry.UnregisterConnection(r.clientID)

	if shouldFinalize {
		r.finalizeCache()
	} else {
		if fileToClose != nil {
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
		}
	}

	return nil
}

func (r *CacheReader) finalizeCache() {
	// Close write handle if open
	r.state.Lock.Lock()
	fileToClose := r.state.File
	r.state.File = nil
	r.state.Lock.Unlock()

	if fileToClose != nil {
		_ = fileToClose.Close()
	}

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

	// Wait until at least some bytes are available from `off`
	for {
		avail := r.state.RangeSet.AvailableFrom(off)
		if avail > 0 || r.state.RangeSet.IsComplete(r.state.TotalSize) {
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
			r.state.Lock.Unlock()
			GlobalRegistry.UpdateConnectionStarvation(r.clientID, -1)
			break
		}

		select {
		case <-r.ctx.Done():
			r.state.Lock.Lock()
			delete(r.state.Starving, r.clientID)
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
			r.state.Lock.Unlock()
			return 0, r.ctx.Err()
		}
	}

	// Read directly from reader's independent file handle
	file, err := r.getFile()
	if err != nil {
		return 0, err
	}

	avail := r.state.RangeSet.AvailableFrom(off)
	toRead := int64(len(p))
	if avail > 0 && toRead > avail {
		toRead = avail
	}

	n, err = file.ReadAt(p[:toRead], off)
	if n > 0 {
		GlobalRegistry.UpdateConnectionOffset(r.clientID, off+int64(n))

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
	return next, nil
}

func FileExists(path string) bool {
	_, err := os.Stat(path)
	return !os.IsNotExist(err)
}
