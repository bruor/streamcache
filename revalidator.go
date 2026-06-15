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

var contentRangeRegex = regexp.MustCompile(`/(\d+)\s*$`)

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
		// Video file exists but metadata is missing. Recreate it dynamically based on the file's ModTime.
		log.Printf("[Revalidate] Metadata missing for cached file %s. Recreating from disk stat...", key)
		err = recreateMetadata(filePath, metaPath, url)
		if err != nil {
			log.Printf("[Revalidate] Failed to recreate metadata for %s: %v. Evicting.", key, err)
			evictCachedFile(filePath, metaPath)
			return false, nil
		}
		// Read it back to continue validation
		metaBytes, err = os.ReadFile(metaPath)
		if err != nil {
			evictCachedFile(filePath, metaPath)
			return false, nil
		}
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

	log.Printf("[Revalidate] Cache validation expired for %s (Age: %s). Probing CDN upstream...", key, age.Truncate(time.Second))

	upstreamSize, upstreamETag, upstreamLastModified, notModified, err := probeUpstream(context.Background(), url, playerHeader, &meta)
	if err != nil {
		log.Printf("[Revalidate] Network probe failed for %s: %v. Soft-failing and serving cache.", key, err)
		return true, nil
	}

	if notModified {
		log.Printf("[Revalidate] CDN returned 304 Not Modified. Cache is valid for %s", key)
	} else {
		// Detect modifications
		modified := false
		reason := ""

		// Only validate size if it is greater than 1. An upstreamSize of 1 or 0 usually indicates
		// a parsed Content-Length of the single-byte range request due to a missing Content-Range header.
		if upstreamSize > 1 && upstreamSize != meta.TotalSize {
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
	}

	// Cache is valid. Update Last-Validated timestamp.
	meta.LastValidated = time.Now().Unix()
	if !notModified {
		if upstreamETag != "" {
			meta.ETag = upstreamETag
		}
		if upstreamLastModified != "" {
			meta.LastModified = upstreamLastModified
		}
	}
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

func recreateMetadata(filePath, metaPath, url string) error {
	stat, err := os.Stat(filePath)
	if err != nil {
		return err
	}

	meta := FileMeta{
		URL:           url,
		TotalSize:     stat.Size(),
		LastValidated: stat.ModTime().Unix(),
		Completed:     true,
	}

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return err
	}

	return os.WriteFile(metaPath, metaBytes, 0644)
}

// probeUpstream attempts to determine the file's size, ETag, and Last-Modified headers.
// It leverages conditional request headers if meta is provided, and dynamically falls back
// to a standard GET request if Range GET requests fail or are blocked.
// Returns:
//   - int64: total size of the file (0 if notModified is true)
//   - string: ETag from upstream
//   - string: Last-Modified from upstream
//   - bool: true if response was 304 Not Modified
//   - error: error if it failed completely
func probeUpstream(ctx context.Context, targetURL string, playerHeader http.Header, meta *FileMeta) (int64, string, string, bool, error) {
	originalHost := GetCleanHost(targetURL)
	preferredMethod := GetProbeMethod(originalHost)

	log.Printf("[Probe] Probing %s (Host: %s, Preferred Method: %s)", targetURL, originalHost, preferredMethod)

	var lastErr error

	// If preferred method is range, try Range: bytes=0-0 first
	if preferredMethod == ProbeMethodRange {
		size, etag, lm, notModified, err := tryRangeProbe(ctx, targetURL, playerHeader, meta)
		if err == nil {
			// Succeeded with range. Ensure cache is set.
			SetProbeMethod(originalHost, ProbeMethodRange)
			return size, etag, lm, notModified, nil
		}
		log.Printf("[Probe] Range probe failed for host %s: %v. Trying standard GET fallback...", originalHost, err)
		lastErr = err
	}

	// Try standard GET fallback (or if standard GET is preferred)
	size, etag, lm, notModified, err := tryStandardProbe(ctx, targetURL, playerHeader, meta)
	if err == nil {
		// Succeeded with standard. Update cache.
		SetProbeMethod(originalHost, ProbeMethodStandard)
		return size, etag, lm, notModified, nil
	}

	// If standard GET failed but we haven't tried Range probe yet, try Range probe as final fallback
	if preferredMethod == ProbeMethodStandard {
		log.Printf("[Probe] Standard probe failed for host %s: %v. Trying Range probe fallback...", originalHost, err)
		size, etag, lm, notModified, rerr := tryRangeProbe(ctx, targetURL, playerHeader, meta)
		if rerr == nil {
			SetProbeMethod(originalHost, ProbeMethodRange)
			return size, etag, lm, notModified, nil
		}
		lastErr = rerr
	} else {
		lastErr = err
	}

	return 0, "", "", false, fmt.Errorf("all probe methods failed; last error: %w", lastErr)
}

func tryRangeProbe(ctx context.Context, targetURL string, playerHeader http.Header, meta *FileMeta) (int64, string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return 0, "", "", false, err
	}

	req.Header.Set("Range", "bytes=0-0")
	if playerHeader != nil {
		CopySafeHeaders(playerHeader, req.Header)
	} else {
		req.Header.Set("User-Agent", "VLC/3.0.18 LibVLC/3.0.18")
		req.Header.Set("Accept", "*/*")
	}

	// Set conditional headers if metadata is present
	if meta != nil {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}

	resp, err := UpstreamHTTPClient.Do(req)
	if err != nil {
		return 0, "", "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return 0, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), true, nil
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, "", "", false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Cache successful target host
	finalHost := GetCleanHost(resp.Request.URL.String())
	if finalHost != "" {
		SetProbeMethod(finalHost, ProbeMethodRange)
	}

	size, err := parseTotalSizeFromHeaders(resp.Header)
	if err != nil {
		return 0, "", "", false, err
	}

	return size, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
}

func tryStandardProbe(ctx context.Context, targetURL string, playerHeader http.Header, meta *FileMeta) (int64, string, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", targetURL, nil)
	if err != nil {
		return 0, "", "", false, err
	}

	if playerHeader != nil {
		CopySafeHeaders(playerHeader, req.Header)
	} else {
		req.Header.Set("User-Agent", "VLC/3.0.18 LibVLC/3.0.18")
		req.Header.Set("Accept", "*/*")
	}

	// Set conditional headers if metadata is present
	if meta != nil {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.LastModified != "" {
			req.Header.Set("If-Modified-Since", meta.LastModified)
		}
	}

	resp, err := UpstreamHTTPClient.Do(req)
	if err != nil {
		return 0, "", "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return 0, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), true, nil
	}

	if resp.StatusCode != http.StatusOK {
		return 0, "", "", false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Cache successful target host
	finalHost := GetCleanHost(resp.Request.URL.String())
	if finalHost != "" {
		SetProbeMethod(finalHost, ProbeMethodStandard)
	}

	// For standard GET, the size is returned in the Content-Length header
	contentLengthStr := resp.Header.Get("Content-Length")
	if contentLengthStr == "" {
		// Fall back to checking Content-Range just in case the server responded with 206 or Content-Range
		contentRange := resp.Header.Get("Content-Range")
		if contentRange != "" {
			matches := contentRangeRegex.FindStringSubmatch(contentRange)
			if len(matches) > 1 {
				size, err := strconv.ParseInt(matches[1], 10, 64)
				if err == nil {
					return size, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
				}
			}
		}
		return 0, "", "", false, fmt.Errorf("Content-Length header is missing in standard probe")
	}

	size, err := strconv.ParseInt(contentLengthStr, 10, 64)
	if err != nil {
		return 0, "", "", false, fmt.Errorf("invalid Content-Length value %q: %w", contentLengthStr, err)
	}

	return size, resp.Header.Get("ETag"), resp.Header.Get("Last-Modified"), false, nil
}
