package main

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var b64RouteRegex = regexp.MustCompile(`^/u/([-_A-Za-z0-9=]+)$`)

func StartServer() {
	mux := http.NewServeMux()

	// Media streams proxy routes
	mux.HandleFunc("/u/", handleMediaStream)
	mux.HandleFunc("/stream", handleMediaStream)

	// Admin / Dashboard static route
	mux.HandleFunc("/", handleDashboardUI)
	mux.HandleFunc("/dashboard/sse", handleSSE)
	mux.HandleFunc("/api/version", handleAPIVersion)
	mux.HandleFunc("/api/hydration/cancel", handleCancelHydration)
	mux.HandleFunc("/api/sessions", handleAPISessions)

	// Live Diagnostics Suite
	mux.HandleFunc("/debug/state", handleDebugState)
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	port := GlobalConfig.GetPort()
	log.Printf("[Server] Starting HTTP Proxy Server on port %s", port)
	if err := http.ListenAndServe(port, mux); err != nil {
		log.Fatalf("[Server] Failed to start server: %v", err)
	}
}

var globalConnIDSeq uint64

func handleMediaStream(w http.ResponseWriter, r *http.Request) {
	username := r.URL.Query().Get("user")
	if username == "" {
		username = r.URL.Query().Get("username")
	}

	var targetURL string

	// 1. Resolve target VOD URL
	if r.URL.Path == "/stream" {
		targetURL = r.URL.Query().Get("url")
	} else {
		matches := b64RouteRegex.FindStringSubmatch(r.URL.Path)
		if len(matches) > 1 {
			decodedBytes, err := base64DecodeSafe(matches[1])
			if err != nil {
				http.Error(w, "Invalid Base64 URL", http.StatusBadRequest)
				return
			}
			targetURL = string(decodedBytes)
		}
	}

	if targetURL == "" {
		http.Error(w, "Missing IPTV VOD URL", http.StatusBadRequest)
		return
	}

	var nextURLs []string
	if rawNext := r.URL.Query()["next_url"]; len(rawNext) > 0 {
		for _, nu := range rawNext {
			for _, part := range strings.Split(nu, ",") {
				trimmed := strings.TrimSpace(part)
				if trimmed != "" {
					nextURLs = append(nextURLs, trimmed)
				}
			}
		}
	}

	if GlobalPrehydrationManager != nil {
		GlobalPrehydrationManager.AutoDiscoverDispatcharrURL(targetURL)
	}

	// 2. Validate Allowed Hosts
	allowedHosts := GlobalConfig.GetAllowedHosts()
	if len(allowedHosts) > 0 {
		parsed, err := url.Parse(targetURL)
		if err != nil {
			http.Error(w, "Invalid target URL format", http.StatusBadRequest)
			return
		}
		allowed := false
		for _, host := range allowedHosts {
			if parsed.Host == host {
				allowed = true
				break
			}
		}
		if !allowed {
			log.Printf("[Server] Blocked request to unauthorized host: %s", parsed.Host)
			http.Error(w, "Forbidden Host", http.StatusForbidden)
			return
		}
	}

	// Generate cache key and extension (stripping query parameters)
	key, ext := getCacheKeyAndExt(targetURL)
	cacheFileName := key + ext
	cacheDir := GlobalConfig.GetCacheDir()
	cacheFilePath := filepath.Join(cacheDir, cacheFileName)

	// Check for legacy extension-less file if the new file is missing
	if !FileExists(cacheFilePath) {
		legacyFilePath := filepath.Join(cacheDir, key)
		if FileExists(legacyFilePath) {
			log.Printf("[Server] Legacy cache file detected for %s. Migrating...", key)
			err := migrateLegacyFile(legacyFilePath, cacheFilePath, targetURL)
			if err != nil {
				log.Printf("[Server] Error migrating legacy file %s: %v", key, err)
			} else {
				log.Printf("[Server] Successfully migrated legacy cache: %s -> %s", key, cacheFileName)
			}
		}
	}

	// 3. Cache HIT Path with Revalidation
	var cacheHit bool
	var cacheMeta *FileMeta
	if GlobalStorage != nil {
		if meta, exists, err := GlobalStorage.Stat(r.Context(), cacheFileName); err == nil && exists {
			cacheHit = true
			cacheMeta = meta
		}
	} else if FileExists(cacheFilePath) {
		cacheHit = true
	}

	if cacheHit {
		valid, err := Revalidate(cacheFileName, targetURL, r.Header)
		if err == nil && valid {
			var reader StorageReadSeeker
			if GlobalStorage != nil {
				reader, err = GlobalStorage.Open(r.Context(), cacheFileName)
			} else {
				f, ferr := os.Open(cacheFilePath)
				if ferr == nil {
					stat, _ := f.Stat()
					reader = &localFileReadSeeker{file: f, size: stat.Size()}
				} else {
					err = ferr
				}
			}

			if err == nil {
				defer reader.Close()

				// Generate unique client ID using atomic counter
				connID := fmt.Sprintf("client-%d", atomic.AddUint64(&globalClientIDSeq, 1))
				remoteAddr := r.RemoteAddr
				userAgent := r.Header.Get("User-Agent")
				if userAgent == "" {
					userAgent = "Unknown"
				}

				totalSize := reader.Size()
				var startOffset int64
				if r != nil {
					startOffset = parseRangeStart(r.Header.Get("Range"))
				}

				conn := &ClientConnection{
					ID:             connID,
					Key:            cacheFileName,
					URL:            targetURL,
					RemoteAddr:     remoteAddr,
					UserAgent:      userAgent,
					Username:       username,
					Offset:         startOffset,
					StartOffset:    startOffset,
					TotalSize:      totalSize,
					StartTime:      time.Now().Unix(),
					InCache:        true,
					StarvingOffset: -1,
				}
				GlobalRegistry.RegisterConnection(connID, conn)
				defer GlobalRegistry.UnregisterConnection(connID)

				// Wrap file in a tracking reader to monitor read offsets
				tracker := &trackingReader{
					reader:            reader,
					lastEvictedOffset: startOffset,
					onRead: func(off int64, n int) {
						GlobalRegistry.UpdateConnectionOffset(connID, off+int64(n))
					},
				}

				w.Header().Set("X-Cache", "HIT")
				w.Header().Set("Accept-Ranges", "bytes")

				if cacheMeta != nil && cacheMeta.ContentType != "" {
					w.Header().Set("Content-Type", cacheMeta.ContentType)
				} else {
					metaPath := cacheFilePath + ".json"
					if metaBytes, err := os.ReadFile(metaPath); err == nil {
						var meta FileMeta
						if json.Unmarshal(metaBytes, &meta) == nil && meta.ContentType != "" {
							w.Header().Set("Content-Type", meta.ContentType)
						}
					}
				}
				if GlobalPrehydrationManager != nil && len(nextURLs) > 0 {
					GlobalPrehydrationManager.OnCacheHit(targetURL, nextURLs)
				}
				http.ServeContent(w, r, cacheFileName, time.Now(), tracker)
				return
			}
		}
	}

	// 4. Cache MISS / Hydration Path
	GlobalRegistry.Lock.Lock()
	state, active := GlobalRegistry.Files[cacheFileName]
	if !active {
		GlobalRegistry.Lock.Unlock()

		if GlobalRegistry.IsProbeFailed(cacheFileName) {
			log.Printf("[Server] Cache MISS for %s bypassed (cached upstream probe failure)", key)
			http.Error(w, "Bad Gateway (Cached Upstream Failure)", http.StatusBadGateway)
			return
		}

		// Probe the upstream CDN to fetch total size, Content-Type, ETag, and Last-Modified headers
		log.Printf("[Server] Cache MISS for %s. Probing upstream CDN for metadata...", key)
		totalSize, probeContentType, etag, lastMod, _, err := probeUpstream(r.Context(), targetURL, r.Header, nil)
		if err != nil {
			log.Printf("[Server] Upstream probe failed: %v", err)
			cooldown := GlobalConfig.GetProbeTransientCooldown()
			errType := "transient"
			if isHTMLProbeError(err) || isPermanentProbeError(err) {
				cooldown = GlobalConfig.GetProbeFailureCooldown()
				errType = "permanent"
			}
			log.Printf("[Server] Recording %s probe failure tombstone for %s (Duration: %s)", errType, key, cooldown)
			GlobalRegistry.RecordFailedProbe(cacheFileName, cooldown)

			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}

		// Fall back to MIME type by file extension if upstream didn't provide a specific video content-type
		if probeContentType == "" || strings.HasPrefix(strings.ToLower(probeContentType), "text/html") || probeContentType == "application/octet-stream" {
			probeContentType = getMimeTypeByExt(ext)
		}

		// Pre-evict cache space to ensure sufficient disk space and cache quota before starting hydration
		GlobalRegistry.PreEvict(totalSize)

		metaPath := cacheFilePath + ".json"
		partPath := filepath.Join(GlobalConfig.GetTmpDir(), cacheFileName+".part")

		// Double-check if another concurrent request created FileCacheState during probe/pre-evict
		GlobalRegistry.Lock.Lock()
		existingState, exists := GlobalRegistry.Files[cacheFileName]
		if exists {
			state = existingState
			GlobalRegistry.Lock.Unlock()
		} else {
			state = NewFileCacheState(cacheFileName, targetURL, cacheFilePath, partPath, metaPath, probeContentType, etag, lastMod, totalSize)
			state.NextURLs = nextURLs

			// Save initial sparse metadata
			meta := FileMeta{
				URL:           targetURL,
				TotalSize:     totalSize,
				ContentType:   probeContentType,
				ETag:          etag,
				LastModified:  lastMod,
				LastValidated: time.Now().Unix(),
				Completed:     false,
			}
			metaBytes, _ := json.Marshal(meta)
			_ = os.WriteFile(metaPath, metaBytes, 0644)

			GlobalRegistry.Files[cacheFileName] = state
			GlobalRegistry.Lock.Unlock()
		}
	} else {
		// Priority Upgrade: if file was prehydrating or yielding, immediately upgrade to live playback!
		state.Lock.Lock()
		wasPrehydrating := state.IsPrehydrating || state.IsYielding
		state.IsPrehydrating = false
		state.IsYielding = false
		if len(nextURLs) > 0 {
			state.NextURLs = nextURLs
		}
		isWriterActive := state.WriterActive
		state.Lock.Unlock()

		if wasPrehydrating {
			log.Printf("[Server] Priority Upgrade: File %s upgraded from prehydration/yielding to Live Playback!", cacheFileName)
			state.SetUpstreamStatus("Connecting")
			if !isWriterActive && !state.RangeSet.IsComplete(state.TotalSize) {
				StartCoordinator(state, r.Header)
			}
		}
		GlobalRegistry.Lock.Unlock()
	}

	// Create request-bound CacheReader
	reader := NewCacheReader(r.Context(), state, r)
	defer reader.Close()

	// Check if all bytes are already completed before starting coordinator
	isCompleted := state.RangeSet.IsComplete(state.TotalSize)

	// Spin up Upstream Download Coordinator if not active and file not fully completed
	if !isCompleted {
		StartCoordinator(state, r.Header)
	}

	// Stream growing media content to client using http.ServeContent
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Accept-Ranges", "bytes")
	state.Lock.Lock()
	if state.ContentType != "" {
		w.Header().Set("Content-Type", state.ContentType)
	}
	state.Lock.Unlock()
	http.ServeContent(w, r, cacheFileName, time.Now(), reader)
}

func handleDebugState(w http.ResponseWriter, r *http.Request) {
	activeSessions := GlobalRegistry.GetActiveSessions()

	GlobalRegistry.Lock.RLock()
	files := make([]*FileCacheState, 0, len(GlobalRegistry.Files))
	for _, fileState := range GlobalRegistry.Files {
		files = append(files, fileState)
	}
	GlobalRegistry.Lock.RUnlock()

	type debugFile struct {
		Key             string           `json:"key"`
		URL             string           `json:"url"`
		TotalSize       int64            `json:"total_size"`
		Ranges          [][]int64        `json:"ranges"`
		DownloadedBytes int64            `json:"downloaded_bytes"`
		ActiveWriter    bool             `json:"active_writer"`
		ActiveClients   int              `json:"active_clients"`
		StarvingList    map[string]int64 `json:"starving_clients"`
	}

	var activeFiles []debugFile
	for _, fileState := range files {
		fileState.Lock.Lock()
		k := fileState.Key
		downloaded := fileState.RangeSet.TotalBytes()
		isCompleted := fileState.RangeSet.IsComplete(fileState.TotalSize)

		if isCompleted && fileState.UpstreamStatus != "Committing" {
			fileState.Lock.Unlock()
			continue
		}

		starvingCopy := make(map[string]int64, len(fileState.Starving))
		for clientID, offset := range fileState.Starving {
			starvingCopy[clientID] = offset
		}

		activeFiles = append(activeFiles, debugFile{
			Key:             k,
			URL:             fileState.URL,
			TotalSize:       fileState.TotalSize,
			Ranges:          fileState.RangeSet.ToSlices(),
			DownloadedBytes: downloaded,
			ActiveWriter:    fileState.WriterActive,
			ActiveClients:   len(fileState.Clients),
			StarvingList:    starvingCopy,
		})
		fileState.Lock.Unlock()
	}

	// Sort active files from most complete to least complete (descending)
	sort.Slice(activeFiles, func(i, j int) bool {
		compI := 0.0
		if activeFiles[i].TotalSize > 0 {
			compI = float64(activeFiles[i].DownloadedBytes) / float64(activeFiles[i].TotalSize)
		}
		compJ := 0.0
		if activeFiles[j].TotalSize > 0 {
			compJ = float64(activeFiles[j].DownloadedBytes) / float64(activeFiles[j].TotalSize)
		}
		if compI == compJ {
			return activeFiles[i].Key < activeFiles[j].Key
		}
		return compI > compJ
	})

	// Sort active sessions by start time (oldest first)
	sort.Slice(activeSessions, func(i, j int) bool {
		return activeSessions[i].StartTime < activeSessions[j].StartTime
	})

	freeSpace := atomic.LoadUint64(&GlobalDiskFreeBytes)

	data := map[string]interface{}{
		"total_cache_size_bytes": GlobalRegistry.GetTotalSize(),
		"max_cache_size_bytes":   GlobalConfig.GetCacheMaxBytes(),
		"disk_free_bytes":        freeSpace,
		"active_downloads":       activeFiles,
		"active_connections":     activeSessions,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func handleAPIVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"version": ServerInstanceID,
	})
}

func handleAPISessions(w http.ResponseWriter, r *http.Request) {
	userFilter := r.URL.Query().Get("user")
	if userFilter == "" {
		userFilter = r.URL.Query().Get("username")
	}

	activeSessions := GlobalRegistry.GetActiveSessions()
	filtered := make([]ClientSession, 0)
	for _, session := range activeSessions {
		if userFilter != "" {
			if strings.EqualFold(session.Username, userFilter) {
				filtered = append(filtered, session)
			}
		} else {
			filtered = append(filtered, session)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"count":    len(filtered),
		"user":     userFilter,
		"sessions": filtered,
	})
}

func handleDashboardUI(w http.ResponseWriter, r *http.Request) {
	// Root endpoint serves index.html dashboard
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		content, err := os.ReadFile("./web/index.html")
		if err != nil {
			http.Error(w, "Dashboard HTML not found", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate, max-age=0, private, s-maxage=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Header().Set("Clear-Site-Data", "\"cache\"")
		w.Header().Set("ETag", fmt.Sprintf("\"%s\"", ServerInstanceID))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}
	http.NotFound(w, r)
}

func base64DecodeSafe(str string) ([]byte, error) {
	// Standard base64 decoding with padding fixes
	str = strings.ReplaceAll(str, "-", "+")
	str = strings.ReplaceAll(str, "_", "/")
	switch len(str) % 4 {
	case 2:
		str += "=="
	case 3:
		str += "="
	}
	return base64.StdEncoding.DecodeString(str)
}

type trackingReader struct {
	reader            StorageReadSeeker
	onRead            func(off int64, n int)
	currOffset        int64
	lastEvictedOffset int64
}

func (tr *trackingReader) ReadAt(p []byte, off int64) (n int, err error) {
	n, err = tr.reader.ReadAt(p, off)
	if n > 0 {
		tr.onRead(off, n)
	}
	return n, err
}

func (tr *trackingReader) Seek(offset int64, whence int) (int64, error) {
	newPos, err := tr.reader.Seek(offset, whence)
	if err == nil {
		atomic.StoreInt64(&tr.currOffset, newPos)
		atomic.StoreInt64(&tr.lastEvictedOffset, newPos)
	}
	return newPos, err
}

func (tr *trackingReader) Read(p []byte) (n int, err error) {
	startOff := atomic.LoadInt64(&tr.currOffset)
	n, err = tr.reader.Read(p)
	if n > 0 {
		atomic.AddInt64(&tr.currOffset, int64(n))
		tr.onRead(startOff, n)
	}
	return n, err
}

func getCacheKeyAndExt(targetURL string) (string, string) {
	parsed, err := url.Parse(targetURL)
	if err != nil {
		// Fallback if URL parsing fails
		key := fmt.Sprintf("%x", md5.Sum([]byte(targetURL)))
		ext := filepath.Ext(targetURL)
		if ext == "" {
			ext = ".mkv"
		}
		// Strip query parameters from extension if present in fallback path
		if idx := strings.Index(ext, "?"); idx != -1 {
			ext = ext[:idx]
		}
		return key, ext
	}

	// Reconstruct the URL without query parameters or fragments
	cleanURL := parsed.Scheme + "://" + parsed.Host + parsed.Path
	key := fmt.Sprintf("%x", md5.Sum([]byte(cleanURL)))

	ext := filepath.Ext(parsed.Path)
	if ext == "" {
		ext = ".mkv"
	}
	return key, ext
}

func migrateLegacyFile(legacyPath, newPath, targetURL string) error {
	// 1. Rename the legacy file to the new path (with extension)
	err := os.Rename(legacyPath, newPath)
	if err != nil {
		return err
	}

	// 2. Create the companion metadata JSON using the shared helper
	metaPath := newPath + ".json"
	return recreateMetadata(newPath, metaPath, targetURL)
}

func handleCancelHydration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")
	if key == "" {
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			key = body.Key
		}
	}

	if key == "" {
		http.Error(w, "Missing 'key' parameter", http.StatusBadRequest)
		return
	}

	GlobalRegistry.Lock.RLock()
	fileState, exists := GlobalRegistry.Files[key]
	if !exists {
		for k, f := range GlobalRegistry.Files {
			if k == key || strings.HasPrefix(k, key+".") || strings.TrimSuffix(k, filepath.Ext(k)) == key {
				fileState = f
				exists = true
				break
			}
		}
	}
	GlobalRegistry.Lock.RUnlock()

	if !exists || fileState == nil {
		http.Error(w, "Hydration not found for specified key", http.StatusNotFound)
		return
	}

	log.Printf("[Server] Received admin cancel request for hydration: %s", key)
	CancelCoordinator(fileState)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Hydration cancelled successfully",
		"key":     key,
	})
}

func getMimeTypeByExt(ext string) string {
	ext = strings.ToLower(ext)
	switch ext {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".ts":
		return "video/mp2t"
	case ".avi":
		return "video/x-msvideo"
	case ".mov":
		return "video/quicktime"
	case ".webm":
		return "video/webm"
	default:
		return "video/x-matroska"
	}
}
