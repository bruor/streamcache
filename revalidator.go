package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var contentRangeRegex = regexp.MustCompile(`/(\d+)$`)

// Revalidate checks if a cached VOD has expired and probes the origin
// to see if the file changed on the CDN.
// Returns:
//   - bool: true if cache is valid (serve it), false if cache is invalid/evicted (must re-download)
//   - error: error if critical validation pipeline fails
func Revalidate(key string, url string, playerHeader http.Header) (bool, error) {
	filePath := filepath.Join(GlobalConfig.CacheDir, key)
	metaPath := filePath + ".json"

	// If cached file doesn't exist, we consider it valid (we'll start a fresh download)
	if !FileExists(filePath) {
		return true, nil
	}

	// Read metadata
	metaBytes, err := os.ReadFile(metaPath)
	if err != nil {
		// Video file exists but metadata is missing. Regard as invalid and recreate.
		log.Printf("[Revalidate] Metadata missing for cached file %s. Evicting.", key)
		evictCachedFile(filePath, metaPath)
		return false, nil
	}

	var meta FileMeta
	err = json.Unmarshal(metaBytes, &meta)
	if err != nil {
		log.Printf("[Revalidate] Metadata corrupted for cached file %s. Evicting.", key)
		evictCachedFile(filePath, metaPath)
		return false, nil
	}

	// Check if the validation time has expired
	age := time.Since(time.Unix(meta.LastValidated, 0))
	if age < GlobalConfig.CacheRevalidateInterval {
		// Fresh validation, skip network probe
		return true, nil
	}

	log.Printf("[Revalidate] Cache validation expired for %s (Age: %s). Probing CDN upstream...", key, age.Truncate(time.Second))

	// Probe upstream using GET bytes=0-0
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		// Soft-fail: keep cache if we can't create the probe request
		return true, nil
	}

	req.Header.Set("Range", "bytes=0-0")
	if playerHeader != nil {
		for k, vv := range playerHeader {
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

	resp, err := UpstreamHTTPClient.Do(req)
	if err != nil {
		log.Printf("[Revalidate] Network probe failed for %s: %v. Soft-failing and serving cache.", key, err)
		return true, nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		log.Printf("[Revalidate] CDN probe returned HTTP status %d. Soft-failing and serving cache.", resp.StatusCode)
		return true, nil
	}

	// Parse upstream size
	upstreamSize, err := parseTotalSizeFromHeaders(resp.Header)
	if err != nil {
		log.Printf("[Revalidate] CDN probe could not determine file size: %v. Soft-failing.", err)
		return true, nil
	}

	upstreamETag := resp.Header.Get("ETag")
	upstreamLastModified := resp.Header.Get("Last-Modified")

	// Detect modifications
	modified := false
	reason := ""

	if upstreamSize != meta.TotalSize {
		modified = true
		reason = fmt.Sprintf("file size changed (cached: %d, upstream: %d)", meta.TotalSize, upstreamSize)
	} else if upstreamETag != "" && meta.ETag != "" && upstreamETag != meta.ETag {
		modified = true
		reason = fmt.Sprintf("ETag changed (cached: %s, upstream: %s)", meta.ETag, upstreamETag)
	} else if upstreamLastModified != "" && meta.LastModified != "" && upstreamLastModified != meta.LastModified {
		modified = true
		reason = fmt.Sprintf("Last-Modified changed (cached: %s, upstream: %s)", meta.LastModified, upstreamLastModified)
	}

	if modified {
		log.Printf("[Revalidate] ALERT: Media content changed on CDN upstream: %s. Evicting cache for %s.", reason, key)
		evictCachedFile(filePath, metaPath)
		return false, nil
	}

	// Cache is valid. Update Last-Validated timestamp.
	meta.LastValidated = time.Now().Unix()
	newMetaBytes, err := json.Marshal(meta)
	if err == nil {
		_ = os.WriteFile(metaPath, newMetaBytes, 0644)
	}

	log.Printf("[Revalidate] Cache validated successfully. Serving HIT for: %s", key)
	return true, nil
}

func evictCachedFile(path, metaPath string) {
	info, err := os.Stat(path)
	var size int64
	if err == nil {
		size = info.Size()
	}

	_ = os.Remove(path)
	_ = os.Remove(metaPath)

	if size > 0 {
		GlobalRegistry.UpdateSize(-size)
	}
}

func parseTotalSizeFromHeaders(header http.Header) (int64, error) {
	// 1. Try Content-Range
	contentRange := header.Get("Content-Range")
	if contentRange != "" {
		matches := contentRangeRegex.FindStringSubmatch(contentRange)
		if len(matches) > 1 {
			size, err := strconv.ParseInt(matches[1], 10, 64)
			if err == nil {
				return size, nil
			}
		}
	}

	// 2. Fall back to Content-Length (only valid if Status was 200, which bytes=0-0 range GET would not be unless CDN ignored range)
	contentLength := header.Get("Content-Length")
	if contentLength != "" {
		size, err := strconv.ParseInt(contentLength, 10, 64)
		if err == nil {
			return size, nil
		}
	}

	return 0, fmt.Errorf("unable to determine total size from headers")
}
